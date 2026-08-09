package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxLineBytes caps a single console line. Minecraft lines are short, but a
// crashing plugin can emit a very long one and bufio.Scanner would otherwise
// fail the whole stream on a 64 KiB default.
const maxLineBytes = 1 << 20

// DefaultShutdownTimeout is how long a server gets to save and exit when the
// daemon is going down. Shorter than an operator-initiated stop: the daemon
// itself is on a clock, and a world that has not finished saving is killed by
// the process group anyway when the process exits.
const DefaultShutdownTimeout = 20 * time.Second

// CommandBuilder produces the executable and arguments used to launch a server.
// It is a field on ProcessRunner so tests can start a harmless fake process
// instead of a real JVM.
type CommandBuilder func(srv *Server) (name string, args []string, err error)

// DefaultCommandBuilder builds the standard `java -Xms -Xmx -jar <jar> nogui`
// invocation.
func DefaultCommandBuilder(srv *Server) (string, []string, error) {
	// A server that is its own program: no JVM, no heap flags, no jar.
	//
	// Two shapes arrive here and they need opposite treatment. The Bedrock
	// server is a file inside the server directory, and passing its bare name
	// would have os/exec look for it on PATH — everywhere except where it is.
	// A PHP interpreter is an absolute path from the runtime manager and lives
	// outside the server directory entirely; joining that with the directory
	// produces a path with two drive letters in it, which is what happened
	// before this told them apart.
	if srv.Executable != "" {
		program := srv.Executable
		if !filepath.IsAbs(program) {
			program = filepath.Join(srv.Dir, program)
		}
		return program, srv.LaunchArgs, nil
	}
	if srv.JarName == "" && len(srv.LaunchArgs) == 0 {
		return "", nil, errors.New("server jar is not set")
	}
	java := srv.JavaBin
	if java == "" {
		java = "java"
	}
	ram := srv.RAMMb
	if ram <= 0 {
		ram = 1024
	}

	args := []string{
		"-Xms" + strconv.Itoa(ram) + "M",
		"-Xmx" + strconv.Itoa(ram) + "M",
	}
	args = append(args, EncodingArgs()...)
	args = append(args, srv.JavaArgs...)
	if len(srv.LaunchArgs) > 0 {
		return java, append(args, srv.LaunchArgs...), nil
	}
	args = append(args, "-jar", srv.JarName, "nogui")
	return java, args, nil
}

// EncodingArgs pins the JVM's streams to UTF-8.
//
// The console reader here assumes the server's output is UTF-8, and on a
// host whose platform charset is not — a Russian Windows Server, a container
// with a POSIX locale — a JVM older than 18 writes its log in that charset
// instead, and every non-ASCII line comes back as replacement characters.
// Java 18 made file.encoding default to UTF-8, but the panel installs
// runtimes as old as 8, so it is set explicitly.
//
// Both spellings are passed on purpose: stdout.encoding is the supported
// property from Java 19, sun.stdout.encoding is what earlier versions read.
// An unrecognised -D is ignored rather than fatal, so passing both costs
// nothing.
//
// Nothing is set for stdin. It was tried, and it turned out not to be needed:
// a command that looked mangled on the way in was mangled by the test shell
// sending cp1251, not by the JVM. Flags that fix nothing are worse than no
// flags, because the next person assumes they are load-bearing.
func EncodingArgs() []string {
	return []string{
		"-Dfile.encoding=UTF-8",
		"-Dstdout.encoding=UTF-8",
		"-Dstderr.encoding=UTF-8",
		"-Dsun.stdout.encoding=UTF-8",
		"-Dsun.stderr.encoding=UTF-8",
	}
}

// ProcessRunner runs each server as a direct child process. It is the runner
// used on Windows and on Linux hosts without Docker.
//
// A kill reaches the whole tree, not just the direct child: see procGroup.
type ProcessRunner struct {
	// Build produces the launch command. Defaults to DefaultCommandBuilder.
	Build CommandBuilder

	// StopCommand is written to stdin for a graceful stop.
	StopCommand string

	// ShutdownTimeout is how long each server gets to stop when the daemon is
	// going down. Zero means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// Env is the environment for launched processes. Nil means inherit the
	// daemon's environment.
	Env []string

	log *slog.Logger

	mu    sync.Mutex
	procs map[string]*process
}

// NewProcessRunner returns a runner ready to start servers.
func NewProcessRunner(log *slog.Logger) *ProcessRunner {
	if log == nil {
		log = slog.Default()
	}
	return &ProcessRunner{
		Build:       DefaultCommandBuilder,
		StopCommand: "stop",
		log:         log,
		procs:       make(map[string]*process),
	}
}

// process is one supervised server.
type process struct {
	id  string
	hub *Hub

	cmd   *exec.Cmd
	stdin io.WriteCloser
	group procGroup

	mu        sync.Mutex
	status    Status
	startedAt time.Time
	// stopping records that a stop was requested, so a clean exit is reported
	// as stopped rather than crashed.
	stopping bool

	// stopWord is what this server understands as "shut down", remembered
	// from the record because Stop is called with an id and nothing else.
	stopWord string

	exited chan struct{} // closed once the process has exited and been reaped
	wg     sync.WaitGroup
}

var _ Runner = (*ProcessRunner)(nil)

// Start launches the server process and begins capturing its output.
func (r *ProcessRunner) Start(ctx context.Context, srv *Server) error {
	if srv == nil || srv.ID == "" {
		return errors.New("server is nil or has no id")
	}

	r.mu.Lock()
	if existing, ok := r.procs[srv.ID]; ok && existing.currentStatus().IsActive() {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	r.mu.Unlock()

	build := r.Build
	if build == nil {
		build = DefaultCommandBuilder
	}
	name, args, err := build(srv)
	if err != nil {
		return fmt.Errorf("building launch command for server %s: %w", srv.ID, err)
	}

	name, dir, err := resolveLaunchPaths(name, srv.Dir)
	if err != nil {
		return fmt.Errorf("resolving launch paths for server %s: %w", srv.ID, err)
	}

	// Launching a named program with arguments is what a server runner does;
	// the name is resolved above and the arguments come from the core's own
	// build step, not from a request.
	cmd := exec.Command(name, args...) // #nosec G204 -- running the server is the purpose
	cmd.Dir = dir
	cmd.Env = r.Env

	group := newProcGroup()
	group.prepare(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("opening stdin for server %s: %w", srv.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opening stdout for server %s: %w", srv.ID, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("opening stderr for server %s: %w", srv.ID, err)
	}

	p := &process{
		id:       srv.ID,
		stopWord: srv.StopCommand,
		hub:      NewHub(ConsoleBufferLines),
		cmd:      cmd,
		stdin:    stdin,
		group:    group,
		status:   StatusStarting,
		exited:   make(chan struct{}),
	}

	r.mu.Lock()
	r.procs[srv.ID] = p
	r.mu.Unlock()

	if err := cmd.Start(); err != nil {
		p.setStatus(StatusCrashed)
		p.hub.Close()
		close(p.exited)
		return fmt.Errorf("starting server %s: %w", srv.ID, err)
	}

	// A server that runs but cannot be killed as a tree is still better than
	// no server, so a failed attach is a warning rather than a refusal — but
	// it is a warning worth seeing, because it means a later kill may leave
	// children holding the port.
	if err := group.attach(cmd); err != nil {
		r.log.Warn("could not group the server process; a kill may leave children behind",
			slog.String("server_id", srv.ID), slog.String("error", err.Error()))
	}

	p.mu.Lock()
	p.startedAt = time.Now().UTC()
	p.mu.Unlock()

	p.wg.Add(2)
	go p.capture(stdout, StreamStdout)
	go p.capture(stderr, StreamStderr)

	// The process is up; readiness detection (parsing the "Done" line) belongs
	// with the core providers in task 1.3, so running is reported on launch.
	p.setStatus(StatusRunning)

	go r.reap(p)

	return nil
}

// resolveLaunchPaths makes the executable and the working directory absolute.
//
// This is not tidiness. os/exec resolves a relative Path against the *calling
// process's* directory, not against Cmd.Dir — so launching
// "data/java/21/bin/java.exe" with Dir set to the server's folder looks
// obviously correct and fails with "the system cannot find the path
// specified", naming a path that plainly exists. The daemon hands out paths
// derived from a data directory that is relative by default, so this is the
// normal case rather than an edge one.
//
// A bare name with no separator is left alone: that is a PATH lookup, which is
// what "java" is meant to be.
func resolveLaunchPaths(name, dir string) (string, string, error) {
	if dir != "" {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return "", "", fmt.Errorf("resolving the server directory %q: %w", dir, err)
		}
		dir = absDir
	}

	if strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
		absName, err := filepath.Abs(name)
		if err != nil {
			return "", "", fmt.Errorf("resolving the launch command %q: %w", name, err)
		}
		name = absName
	}
	return name, dir, nil
}

// capture reads one stream line by line into the hub.
func (p *process) capture(rc io.ReadCloser, stream string) {
	defer p.wg.Done()
	defer func() { _ = rc.Close() }()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		p.hub.Publish(ConsoleLine{
			TS:     time.Now().UTC(),
			Stream: stream,
			Text:   strings.TrimRight(scanner.Text(), "\r"),
		})
	}
}

// reap waits for the process to exit, settles the final status and tears down
// every console subscription.
func (r *ProcessRunner) reap(p *process) {
	// Wait for the readers to drain the pipes before Wait closes them,
	// otherwise the tail of the log is lost.
	p.wg.Wait()
	err := p.cmd.Wait()

	final := StatusStopped
	if err != nil && !p.wasStopping() {
		final = StatusCrashed
		r.log.Warn("server process exited unexpectedly",
			slog.String("server_id", p.id), slog.String("error", err.Error()))
	}
	p.setStatus(final)

	// Released after the process is reaped, not before: on Windows the job
	// handle is what keeps the group alive, and closing it early would kill a
	// server that was merely being waited on.
	if err := p.group.close(); err != nil {
		r.log.Debug("releasing the process group failed",
			slog.String("server_id", p.id), slog.String("error", err.Error()))
	}

	close(p.exited)
	p.hub.Close()
}

func (p *process) setStatus(s Status) {
	p.mu.Lock()
	changed := p.status != s
	p.status = s
	p.mu.Unlock()

	if changed {
		p.hub.PublishStatus(s)
	}
}

func (p *process) currentStatus() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *process) wasStopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopping
}

// lookup returns the supervised process for id.
func (r *ProcessRunner) lookup(id string) (*process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.procs[id]
	if !ok {
		return nil, ErrServerNotFound
	}
	return p, nil
}

// Stop asks the server to shut down gracefully by writing the stop command to
// stdin, then falls back to Kill if it has not exited within timeout.
func (r *ProcessRunner) Stop(ctx context.Context, id string, timeout time.Duration) error {
	p, err := r.lookup(id)
	if err != nil {
		return err
	}
	if !p.currentStatus().IsActive() {
		return ErrNotRunning
	}

	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.setStatus(StatusStopping)

	word := p.stopWord
	if word == "" {
		word = r.StopCommand
	}
	if _, err := io.WriteString(p.stdin, word+"\n"); err != nil {
		// stdin is already gone; go straight to killing the process.
		r.log.Warn("writing stop command failed, killing instead",
			slog.String("server_id", id), slog.String("error", err.Error()))
		return r.Kill(ctx, id)
	}

	select {
	case <-p.exited:
		return nil
	case <-time.After(timeout):
		r.log.Warn("graceful stop timed out, killing",
			slog.String("server_id", id), slog.Duration("timeout", timeout))
		return r.Kill(ctx, id)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Kill terminates the process immediately.
func (r *ProcessRunner) Kill(ctx context.Context, id string) error {
	p, err := r.lookup(id)
	if err != nil {
		return err
	}

	select {
	case <-p.exited:
		return ErrNotRunning
	default:
	}

	if p.cmd.Process == nil {
		return ErrNotRunning
	}
	// The whole group, not just the direct child: a JVM started through a
	// wrapper, or a modded server that forks a helper, leaves children holding
	// the port and the world files behind otherwise.
	if err := p.group.kill(p.cmd); err != nil {
		return fmt.Errorf("killing server %s: %w", id, err)
	}

	select {
	case <-p.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status reports the current lifecycle state.
func (r *ProcessRunner) Status(ctx context.Context, id string) (Status, error) {
	p, err := r.lookup(id)
	if err != nil {
		return "", err
	}
	return p.currentStatus(), nil
}

// History returns buffered console scrollback, oldest first.
func (r *ProcessRunner) History(ctx context.Context, id string, lines int) ([]ConsoleLine, error) {
	p, err := r.lookup(id)
	if err != nil {
		return nil, err
	}
	return p.hub.History(lines), nil
}

// Subscribe streams new console lines for the server.
func (r *ProcessRunner) Subscribe(ctx context.Context, id string) (<-chan ConsoleLine, func(), error) {
	p, err := r.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	ch, cancel := p.hub.Subscribe(ctx)
	return ch, cancel, nil
}

// SubscribeWithHistory returns scrollback and a live stream in one atomic step.
func (r *ProcessRunner) SubscribeWithHistory(ctx context.Context, id string, lines int) ([]ConsoleLine, <-chan ConsoleLine, func(), error) {
	p, err := r.lookup(id)
	if err != nil {
		return nil, nil, nil, err
	}
	history, ch, cancel := p.hub.SubscribeWithHistory(ctx, lines)
	return history, ch, cancel, nil
}

// SubscribeStatus streams status changes for the server.
func (r *ProcessRunner) SubscribeStatus(ctx context.Context, id string) (<-chan Status, func(), error) {
	p, err := r.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	ch, cancel := p.hub.SubscribeStatus(ctx)
	return ch, cancel, nil
}

// SendCommand validates cmd and writes it to the server's stdin.
func (r *ProcessRunner) SendCommand(ctx context.Context, id string, cmd string) error {
	if err := ValidateCommand(cmd); err != nil {
		return err
	}

	p, err := r.lookup(id)
	if err != nil {
		return err
	}
	if !p.currentStatus().IsActive() {
		return ErrNotRunning
	}

	if _, err := io.WriteString(p.stdin, cmd+"\n"); err != nil {
		return fmt.Errorf("writing command to server %s: %w", id, err)
	}
	return nil
}

// Shutdown stops every running server and releases every console
// subscription, so WebSocket handlers unblock and the daemon can exit cleanly.
//
// Stopping the servers is not optional politeness. Each one is held in a
// process group that dies with the daemon, so a daemon that exited without
// stopping them would take every world down with a kill — no "Saving worlds",
// no clean shutdown, whatever was in memory since the last autosave gone.
// Asking each to stop first turns that into the ordinary shutdown a server
// expects.
//
// Servers are stopped in parallel: ten worlds saving one after another would
// not fit in a shutdown timeout that ten worlds saving at once fits easily.
func (r *ProcessRunner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	procs := make([]*process, 0, len(r.procs))
	for _, p := range r.procs {
		procs = append(procs, p)
	}
	r.mu.Unlock()

	timeout := r.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	// Whatever the caller allows, minus a margin to close the sockets in. A
	// stop that used the whole budget would leave nothing for the rest.
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline) - time.Second; remaining < timeout {
			timeout = remaining
		}
	}

	var wg sync.WaitGroup
	for _, p := range procs {
		if !p.currentStatus().IsActive() {
			continue
		}
		wg.Add(1)
		go func(p *process) {
			defer wg.Done()
			if err := r.Stop(ctx, p.id, timeout); err != nil {
				r.log.Warn("stopping a server during shutdown failed",
					slog.String("server_id", p.id), slog.String("error", err.Error()))
			}
		}(p)
	}
	wg.Wait()

	for _, p := range procs {
		p.hub.Close()
	}
	return nil
}
