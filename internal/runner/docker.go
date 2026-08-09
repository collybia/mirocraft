package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/docker"
)

// Container conventions.
const (
	// ContainerPrefix names a server's container. A stable, derivable name is
	// what lets a restarted daemon find the servers it left running: unlike a
	// child process, a container outlives the daemon that made it.
	ContainerPrefix = "mirocraft-"

	// ManagedLabel marks containers this panel owns, so it never touches
	// anything else on the host.
	ManagedLabel = "tech.mirocraft.managed"
	// ServerIDLabel records which server a container belongs to.
	ServerIDLabel = "tech.mirocraft.server-id"

	// containerDataDir is where a server's directory is mounted inside the
	// container.
	containerDataDir = "/data"
)

// DefaultImage returns the image that runs a given Java version.
//
// Eclipse Temurin because that is the runtime the panel installs on the
// process path too, so a server behaves the same whichever runner started it.
// The JRE variant, not the JDK: a Minecraft server does not compile anything,
// and the JDK is roughly twice the download.
func DefaultImage(javaMajor int) string {
	if javaMajor <= 0 {
		javaMajor = 21
	}
	return "eclipse-temurin:" + strconv.Itoa(javaMajor) + "-jre"
}

// DockerRunner runs each server in its own container. It is the preferred
// runner on Linux: the server gets a real memory limit, a clean filesystem
// view and a Java runtime that does not have to be installed on the host.
type DockerRunner struct {
	client *docker.Client

	// Image chooses the image for a Java version. Defaults to DefaultImage.
	Image func(javaMajor int) string
	// CommandFor builds the command run inside the container. Defaults to
	// containerCommand; a field so tests can start something harmless in place
	// of a JVM, the same way ProcessRunner.Build exists.
	CommandFor func(srv *Server) []string
	// StopCommand is written to stdin for a graceful stop.
	StopCommand string
	// ShutdownTimeout is how long each server gets to stop when the daemon is
	// going down.
	ShutdownTimeout time.Duration
	// CPUs limits container CPU time, in cores. Zero means unlimited.
	CPUs float64

	log *slog.Logger

	mu         sync.Mutex
	containers map[string]*container
}

var _ Runner = (*DockerRunner)(nil)

// NewDockerRunner returns a runner backed by the Docker daemon at host. An
// empty host means the platform default.
func NewDockerRunner(host string, log *slog.Logger) (*DockerRunner, error) {
	if log == nil {
		log = slog.Default()
	}

	client, err := docker.New(host)
	if err != nil {
		return nil, err
	}

	return &DockerRunner{
		client:      client,
		Image:       DefaultImage,
		CommandFor:  containerCommand,
		StopCommand: "stop",
		log:         log,
		containers:  make(map[string]*container),
	}, nil
}

// Available reports whether the Docker daemon answers. This is what runner
// auto-selection asks before choosing it.
func (r *DockerRunner) Available(ctx context.Context) error { return r.client.Ping(ctx) }

// Client exposes the underlying client, for the daemon's startup logging.
func (r *DockerRunner) Client() *docker.Client { return r.client }

// container is one supervised server.
type container struct {
	id          string // the panel's server id
	containerID string
	hub         *Hub

	// conn is the attached stream: the console reads from it and commands are
	// written to it. One connection carries both, which is what the Engine
	// gives and what keeps the two from getting out of step.
	conn net.Conn

	// stopWord is what this server understands as "shut down", remembered from
	// the record because Stop is called with an id and nothing else.
	stopWord string

	mu        sync.Mutex
	status    Status
	startedAt time.Time
	stopping  bool

	exited chan struct{}
	wg     sync.WaitGroup
}

func (c *container) setStatus(s Status) {
	c.mu.Lock()
	changed := c.status != s
	c.status = s
	c.mu.Unlock()

	if changed {
		c.hub.PublishStatus(s)
	}
}

func (c *container) currentStatus() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *container) wasStopping() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopping
}

// Start creates the container and begins capturing its output.
func (r *DockerRunner) Start(ctx context.Context, srv *Server) error {
	if srv == nil || srv.ID == "" {
		return errors.New("server is nil or has no id")
	}

	r.mu.Lock()
	if existing, ok := r.containers[srv.ID]; ok && existing.currentStatus().IsActive() {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	r.mu.Unlock()

	name := ContainerPrefix + srv.ID

	// A container with this name may still exist from a previous run. If it is
	// running the server is already up — including under a daemon that has
	// since restarted — and if it is not, it is a corpse in the way of the new
	// one.
	if existing, err := r.client.InspectContainer(ctx, name); err == nil {
		if existing.State.Running {
			return ErrAlreadyRunning
		}
		if err := r.client.RemoveContainer(ctx, name, true); err != nil {
			return fmt.Errorf("removing the previous container for server %s: %w", srv.ID, err)
		}
	} else if !errors.Is(err, docker.ErrNotFound) {
		return err
	}

	ref := srv.Image
	if ref == "" {
		image := r.Image
		if image == nil {
			image = DefaultImage
		}
		ref = image(srv.JavaMajor)
	}

	if err := r.ensureImage(ctx, ref); err != nil {
		return err
	}

	dir, err := filepath.Abs(srv.Dir)
	if err != nil {
		return fmt.Errorf("resolving the directory of server %s: %w", srv.ID, err)
	}

	command := r.CommandFor
	if command == nil {
		command = containerCommand
	}

	spec := docker.ContainerSpec{
		Name:    name,
		Image:   ref,
		Cmd:     command(srv),
		WorkDir: containerDataDir,
		Binds:   []string{dir + ":" + containerDataDir},
		Labels: map[string]string{
			ManagedLabel:  "1",
			ServerIDLabel: srv.ID,
		},
		// The container's own limit, rather than the JVM's -Xmx alone: a
		// server that leaks past its heap takes the host down with it
		// otherwise. Given headroom because the JVM needs memory outside the
		// heap — metaspace, thread stacks, direct buffers — and a limit equal
		// to -Xmx is an OOM kill waiting for the first busy day.
		MemoryBytes: int64(srv.RAMMb+containerMemoryHeadroomMb) << 20,
		User:        containerUser(),
	}
	if srv.Port > 0 {
		if srv.UDP {
			spec.UDPPorts = map[int]int{srv.Port: srv.Port}
		} else {
			spec.Ports = map[int]int{srv.Port: srv.Port}
		}
	}
	if r.CPUs > 0 {
		spec.NanoCPUs = int64(r.CPUs * 1e9)
	}

	containerID, err := r.client.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating the container for server %s: %w", srv.ID, err)
	}

	c := &container{
		id:          srv.ID,
		stopWord:    srv.StopCommand,
		containerID: containerID,
		hub:         NewHub(ConsoleBufferLines),
		status:      StatusStarting,
		exited:      make(chan struct{}),
	}

	// Attached before the start, so the first lines of the boot log are not
	// lost to the race between starting and connecting.
	conn, err := r.client.Attach(context.WithoutCancel(ctx), containerID)
	if err != nil {
		_ = r.client.RemoveContainer(ctx, containerID, true)
		c.hub.Close()
		return fmt.Errorf("attaching to the container of server %s: %w", srv.ID, err)
	}
	c.conn = conn

	r.mu.Lock()
	r.containers[srv.ID] = c
	r.mu.Unlock()

	if err := r.client.StartContainer(ctx, containerID); err != nil {
		_ = conn.Close()
		c.setStatus(StatusCrashed)
		c.hub.Close()
		close(c.exited)
		_ = r.client.RemoveContainer(ctx, containerID, true)
		return fmt.Errorf("starting the container for server %s: %w", srv.ID, err)
	}

	c.mu.Lock()
	c.startedAt = time.Now().UTC()
	c.mu.Unlock()

	// Both goroutines follow the container, not the request that started it:
	// a console reader cancelled when the HTTP handler returns would stop
	// reading the moment the server came up.
	c.wg.Add(1)
	go r.capture(c) // #nosec G118 -- outlives the request on purpose

	c.setStatus(StatusRunning)
	go r.reap(c) // #nosec G118 -- outlives the request on purpose

	return nil
}

// containerMemoryHeadroomMb is added to the requested heap for everything the
// JVM allocates outside it.
const containerMemoryHeadroomMb = 512

// containerCommand builds the java invocation run inside the container.
//
// No path to a java binary: the image provides it on PATH, which is the point
// of using an image at all. The jar is named relative to the mounted data
// directory, which is also the working directory.
func containerCommand(srv *Server) []string {
	ram := srv.RAMMb
	if ram <= 0 {
		ram = 1024
	}
	jar := srv.JarName
	if jar == "" {
		jar = "server.jar"
	}

	// A native server is its own program: no JVM, no heap flags.
	if srv.Executable != "" {
		return append([]string{"./" + srv.Executable}, srv.LaunchArgs...)
	}

	cmd := []string{
		"java",
		"-Xms" + strconv.Itoa(ram) + "M",
		"-Xmx" + strconv.Itoa(ram) + "M",
	}
	cmd = append(cmd, EncodingArgs()...)
	cmd = append(cmd, srv.JavaArgs...)
	if len(srv.LaunchArgs) > 0 {
		return append(cmd, srv.LaunchArgs...)
	}
	return append(cmd, "-jar", jar, "nogui")
}

// ensureImage pulls the image if the host does not have it.
func (r *DockerRunner) ensureImage(ctx context.Context, ref string) error {
	exists, err := r.client.ImageExists(ctx, ref)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	r.log.Info("pulling a runtime image", slog.String("image", ref))
	if err := r.client.PullImage(ctx, ref, nil); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// capture reads the multiplexed stream into the hub.
func (r *DockerRunner) capture(c *container) {
	defer c.wg.Done()

	// The Engine frames output rather than sending raw bytes, and a frame does
	// not align with a line, so lines are assembled here.
	var pending [3][]byte

	for {
		stream, payload, err := docker.DemuxFrame(c.conn)
		if err != nil {
			// Flush whatever was left without a trailing newline: the last
			// line of a crash log usually is.
			for streamID, buffered := range pending {
				if len(buffered) > 0 {
					c.hub.Publish(ConsoleLine{
						TS: time.Now().UTC(), Stream: streamName(streamID),
						Text: strings.TrimRight(string(buffered), "\r"),
					})
				}
			}
			return
		}
		if stream < 0 || stream >= len(pending) {
			continue
		}

		pending[stream] = append(pending[stream], payload...)
		for {
			index := strings.IndexByte(string(pending[stream]), '\n')
			if index < 0 {
				break
			}
			text := strings.TrimRight(string(pending[stream][:index]), "\r")
			pending[stream] = pending[stream][index+1:]

			c.hub.Publish(ConsoleLine{
				TS: time.Now().UTC(), Stream: streamName(stream), Text: text,
			})
		}
	}
}

func streamName(stream int) string {
	if stream == docker.StreamStderr {
		return StreamStderr
	}
	return StreamStdout
}

// reap waits for the container to exit and settles the final status.
func (r *DockerRunner) reap(c *container) {
	code, err := r.client.WaitContainer(context.Background(), c.containerID)

	// The stream ends when the container does; waiting for the reader keeps
	// the tail of the log.
	_ = c.conn.Close()
	c.wg.Wait()

	final := StatusStopped
	switch {
	case err != nil:
		final = StatusCrashed
		r.log.Warn("waiting for a container failed",
			slog.String("server_id", c.id), slog.String("error", err.Error()))
	case code != 0 && !c.wasStopping():
		final = StatusCrashed
		r.log.Warn("server container exited unexpectedly",
			slog.String("server_id", c.id), slog.Int("exit_code", code))
	}
	c.setStatus(final)

	close(c.exited)
	c.hub.Close()
}

func (r *DockerRunner) lookup(id string) (*container, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.containers[id]
	if !ok {
		return nil, ErrServerNotFound
	}
	return c, nil
}

// Stop asks the server to shut down by writing the stop command to its stdin,
// then kills the container if it has not exited within timeout.
//
// The stop command rather than the container's own stop signal: SIGTERM to a
// Minecraft server is a much blunter instrument than "stop", which saves the
// world and closes the region files properly.
func (r *DockerRunner) Stop(ctx context.Context, id string, timeout time.Duration) error {
	c, err := r.lookup(id)
	if err != nil {
		return err
	}
	if !c.currentStatus().IsActive() {
		return ErrNotRunning
	}

	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
	c.setStatus(StatusStopping)

	word := c.stopWord
	if word == "" {
		word = r.StopCommand
	}
	if _, err := io.WriteString(c.conn, word+"\n"); err != nil {
		r.log.Warn("writing the stop command failed, killing instead",
			slog.String("server_id", id), slog.String("error", err.Error()))
		return r.Kill(ctx, id)
	}

	select {
	case <-c.exited:
		return nil
	case <-time.After(timeout):
		r.log.Warn("graceful stop timed out, killing",
			slog.String("server_id", id), slog.Duration("timeout", timeout))
		return r.Kill(ctx, id)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Kill terminates the container immediately.
func (r *DockerRunner) Kill(ctx context.Context, id string) error {
	c, err := r.lookup(id)
	if err != nil {
		return err
	}

	select {
	case <-c.exited:
		return ErrNotRunning
	default:
	}

	// Killing the container reaches everything inside it: that is what a
	// container is, and it is why the Docker path needs no equivalent of the
	// process-group handling the direct-process path does.
	if err := r.client.KillContainer(ctx, c.containerID); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return ErrNotRunning
		}
		return fmt.Errorf("killing the container of server %s: %w", id, err)
	}

	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status reports the current lifecycle state.
func (r *DockerRunner) Status(ctx context.Context, id string) (Status, error) {
	c, err := r.lookup(id)
	if err != nil {
		return "", err
	}
	return c.currentStatus(), nil
}

// History returns buffered console scrollback, oldest first.
func (r *DockerRunner) History(ctx context.Context, id string, lines int) ([]ConsoleLine, error) {
	c, err := r.lookup(id)
	if err != nil {
		return nil, err
	}
	return c.hub.History(lines), nil
}

// Subscribe streams new console lines for the server.
func (r *DockerRunner) Subscribe(ctx context.Context, id string) (<-chan ConsoleLine, func(), error) {
	c, err := r.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	ch, cancel := c.hub.Subscribe(ctx)
	return ch, cancel, nil
}

// SubscribeWithHistory returns scrollback and a live stream in one atomic step.
func (r *DockerRunner) SubscribeWithHistory(ctx context.Context, id string, lines int) ([]ConsoleLine, <-chan ConsoleLine, func(), error) {
	c, err := r.lookup(id)
	if err != nil {
		return nil, nil, nil, err
	}
	history, ch, cancel := c.hub.SubscribeWithHistory(ctx, lines)
	return history, ch, cancel, nil
}

// SubscribeStatus streams status changes for the server.
func (r *DockerRunner) SubscribeStatus(ctx context.Context, id string) (<-chan Status, func(), error) {
	c, err := r.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	ch, cancel := c.hub.SubscribeStatus(ctx)
	return ch, cancel, nil
}

// SendCommand validates cmd and writes it to the container's stdin.
func (r *DockerRunner) SendCommand(ctx context.Context, id string, cmd string) error {
	if err := ValidateCommand(cmd); err != nil {
		return err
	}

	c, err := r.lookup(id)
	if err != nil {
		return err
	}
	if !c.currentStatus().IsActive() {
		return ErrNotRunning
	}

	if _, err := io.WriteString(c.conn, cmd+"\n"); err != nil {
		return fmt.Errorf("writing a command to server %s: %w", id, err)
	}
	return nil
}

// Stats reports resource usage for a running server.
//
// Sampling failures are not errors, for the same reason they are not on the
// process path: a container can stop between the lookup and the sample, and a
// half-filled Stats with a correct uptime beats a hard failure that blanks the
// whole server page.
func (r *DockerRunner) Stats(ctx context.Context, id string) (Stats, error) {
	c, err := r.lookup(id)
	if err != nil {
		return Stats{}, err
	}

	c.mu.Lock()
	startedAt := c.startedAt
	c.mu.Unlock()

	stats := Stats{StartedAt: startedAt}
	if !startedAt.IsZero() {
		stats.Uptime = time.Since(startedAt)
	}
	if !c.currentStatus().IsActive() {
		return stats, nil
	}

	sample, err := r.client.ContainerStats(ctx, c.containerID)
	if err != nil {
		// Statistics are a nicety: a container that stopped between the check
		// above and this call should leave the panel showing a server without
		// numbers, not a server with an error.
		return stats, nil //nolint:nilerr // missing statistics are not a failure
	}
	stats.RAMBytes = sample.MemoryBytes
	stats.CPUPercent = sample.CPUPercent

	// No PID: the server's process id belongs to the container's namespace and
	// means nothing on the host. Reporting one would be worse than reporting
	// none, because it would look usable.
	return stats, nil
}

// Adopt takes over containers left by a previous daemon.
//
// Unlike a child process, a container outlives the daemon that started it, so
// a restarted daemon that assumed everything was gone would report running
// servers as stopped and then fail to start them because the name is taken.
// Called at startup, before anything reads a server's status.
func (r *DockerRunner) Adopt(ctx context.Context) error {
	found, err := r.client.ListContainers(ctx, ManagedLabel+"=1")
	if err != nil {
		return fmt.Errorf("listing managed containers: %w", err)
	}

	for _, info := range found {
		serverID := info.Config.Labels[ServerIDLabel]
		if serverID == "" {
			continue
		}
		if !info.State.Running {
			// A stopped container holds a name the next start needs, and
			// nothing else. Removed rather than kept: its logs are already in
			// no one's scrollback, since the hub that held them died with the
			// daemon.
			if err := r.client.RemoveContainer(ctx, info.ID, false); err != nil {
				r.log.Debug("removing a stopped container failed",
					slog.String("server_id", serverID), slog.String("error", err.Error()))
			}
			continue
		}

		c := &container{
			id: serverID, containerID: info.ID,
			hub: NewHub(ConsoleBufferLines), status: StatusRunning,
			exited: make(chan struct{}),
		}
		// From the container rather than from now: an adopted server that has
		// been up for a week must not report an uptime of zero seconds just
		// because the daemon has only known it since the restart.
		if startedAt, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil {
			c.startedAt = startedAt.UTC()
		}

		conn, err := r.client.Attach(ctx, info.ID)
		if err != nil {
			r.log.Warn("could not reattach to a running server; its console will be empty",
				slog.String("server_id", serverID), slog.String("error", err.Error()))
			c.hub.Close()
			continue
		}
		c.conn = conn

		r.mu.Lock()
		r.containers[serverID] = c
		r.mu.Unlock()

		c.wg.Add(1)
		go r.capture(c) // #nosec G118 -- follows the container, not the caller
		go r.reap(c)    // #nosec G118 -- follows the container, not the caller

		r.log.Info("adopted a running server container",
			slog.String("server_id", serverID), slog.String("container", info.ID[:12]))
	}
	return nil
}

// RunningServers reports the ids of servers this runner currently supervises,
// so the daemon can reconcile its records with reality.
func (r *DockerRunner) RunningServers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string
	for id, c := range r.containers {
		if c.currentStatus().IsActive() {
			out = append(out, id)
		}
	}
	return out
}

// Shutdown stops every running server and releases every subscription.
//
// A container would survive the daemon, so this is politeness rather than the
// necessity it is on the process path — but a server left running while the
// panel is down is a server nobody can see, and adopting it back is only
// possible because it was left cleanly.
func (r *DockerRunner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	containers := make([]*container, 0, len(r.containers))
	for _, c := range r.containers {
		containers = append(containers, c)
	}
	r.mu.Unlock()

	timeout := r.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline) - time.Second; remaining < timeout {
			timeout = remaining
		}
	}

	var wg sync.WaitGroup
	for _, c := range containers {
		if !c.currentStatus().IsActive() {
			continue
		}
		wg.Add(1)
		go func(c *container) {
			defer wg.Done()
			if err := r.Stop(ctx, c.id, timeout); err != nil {
				r.log.Warn("stopping a server during shutdown failed",
					slog.String("server_id", c.id), slog.String("error", err.Error()))
			}
		}(c)
	}
	wg.Wait()

	for _, c := range containers {
		c.hub.Close()
	}
	return nil
}
