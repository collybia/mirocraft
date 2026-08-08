package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// The fake server is this test binary re-executed in a special mode: it echoes
// stdin to stdout the way a Minecraft server echoes commands into its log, and
// exits on "stop". Using the test binary itself keeps the suite free of shell
// scripts and works identically on Windows and Linux.
const fakeServerEnv = "MIROCRAFT_FAKE_SERVER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeServerEnv); mode != "" {
		runFakeServer(mode)
		return
	}
	os.Exit(m.Run())
}

func runFakeServer(mode string) {
	out := os.Stdout
	_, _ = io.WriteString(out, "[INFO] fake server starting\n")
	_, _ = io.WriteString(os.Stderr, "[WARN] this line goes to stderr\n")

	switch mode {
	case "crash":
		os.Exit(3)
	case "ignore-stop":
		// Never reads stdin: exercises the graceful-stop timeout and the fall
		// back to Kill. A plain `select {}` would not do — the runtime spots
		// the deadlock and exits, which is the opposite of what we need.
		time.Sleep(time.Hour)
		os.Exit(0)
	}

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				i := strings.IndexByte(string(buf), '\n')
				if i < 0 {
					break
				}
				cmd := strings.TrimRight(string(buf[:i]), "\r")
				buf = buf[i+1:]

				if cmd == "stop" {
					_, _ = io.WriteString(out, "[INFO] Stopping server\n")
					os.Exit(0)
				}
				_, _ = io.WriteString(out, "[INFO] echo: "+cmd+"\n")
			}
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

// newTestRunner returns a ProcessRunner that launches the fake server instead
// of a JVM.
func newTestRunner(t *testing.T, mode string) *ProcessRunner {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}

	r := NewProcessRunner(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Build = func(srv *Server) (string, []string, error) {
		return self, nil, nil
	}
	r.Env = append(os.Environ(), fakeServerEnv+"="+mode)
	return r
}

func testServer(t *testing.T) *Server {
	t.Helper()
	return &Server{ID: "01TEST", Name: "test", Dir: t.TempDir()}
}

// waitForLine reads from ch until a line containing want appears.
func waitForLine(t *testing.T, ch <-chan ConsoleLine, want string, timeout time.Duration) ConsoleLine {
	t.Helper()

	deadline := time.After(timeout)
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				t.Fatalf("console channel closed while waiting for %q", want)
			}
			if strings.Contains(l.Text, want) {
				return l
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a console line containing %q", want)
		}
	}
}

func TestProcessRunnerCapturesStdoutAndStderr(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() { _ = r.Kill(context.Background(), srv.ID) })

	// Startup lines land in the buffer before anyone subscribes, so they are
	// checked through the history rather than the live stream.
	deadline := time.Now().Add(5 * time.Second)
	var history []ConsoleLine
	for time.Now().Before(deadline) {
		var err error
		history, err = r.History(ctx, srv.ID, 100)
		if err != nil {
			t.Fatalf("reading history: %v", err)
		}
		if len(history) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var sawStdout, sawStderr bool
	for _, l := range history {
		if l.Stream == StreamStdout && strings.Contains(l.Text, "fake server starting") {
			sawStdout = true
		}
		if l.Stream == StreamStderr && strings.Contains(l.Text, "goes to stderr") {
			sawStderr = true
		}
		if l.TS.IsZero() {
			t.Errorf("console line %q has a zero timestamp", l.Text)
		}
	}
	if !sawStdout {
		t.Errorf("stdout line missing from history: %v", texts(history))
	}
	if !sawStderr {
		t.Errorf("stderr line missing from history: %v", texts(history))
	}
}

func TestProcessRunnerSendCommandRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() { _ = r.Kill(context.Background(), srv.ID) })

	ch, unsubscribe, err := r.Subscribe(ctx, srv.ID)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer unsubscribe()

	if err := r.SendCommand(ctx, srv.ID, "say hello"); err != nil {
		t.Fatalf("sending command: %v", err)
	}

	got := waitForLine(t, ch, "echo: say hello", 5*time.Second)
	if got.Stream != StreamStdout {
		t.Errorf("echoed line came from %q, want %q", got.Stream, StreamStdout)
	}

	// The same line must also be retrievable from the scrollback.
	history, err := r.History(ctx, srv.ID, 1000)
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	found := false
	for _, l := range history {
		if strings.Contains(l.Text, "echo: say hello") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("command echo missing from history: %v", texts(history))
	}
}

func TestProcessRunnerRejectsInvalidCommand(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() { _ = r.Kill(context.Background(), srv.ID) })

	if err := r.SendCommand(ctx, srv.ID, "say hi\nop Steve"); !errors.Is(err, ErrCommandControl) {
		t.Fatalf("SendCommand with a newline = %v, want ErrCommandControl", err)
	}
	if err := r.SendCommand(ctx, srv.ID, "  "); !errors.Is(err, ErrCommandEmpty) {
		t.Fatalf("SendCommand with blanks = %v, want ErrCommandEmpty", err)
	}
}

func TestProcessRunnerGracefulStop(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}

	statusCh, unsubscribe, err := r.SubscribeStatus(ctx, srv.ID)
	if err != nil {
		t.Fatalf("subscribing to status: %v", err)
	}
	defer unsubscribe()

	if err := r.Stop(ctx, srv.ID, 5*time.Second); err != nil {
		t.Fatalf("stopping server: %v", err)
	}

	status, err := r.Status(ctx, srv.ID)
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != StatusStopped {
		t.Fatalf("status after graceful stop = %q, want %q", status, StatusStopped)
	}

	// stopping must have been announced before the process went away.
	sawStopping := false
	for {
		select {
		case s, ok := <-statusCh:
			if !ok {
				if !sawStopping {
					t.Fatal("status stream closed without ever reporting stopping")
				}
				return
			}
			if s == StatusStopping {
				sawStopping = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out reading status changes")
		}
	}
}

// A server that ignores the stop command must be killed once the timeout runs
// out, rather than hanging the caller.
func TestProcessRunnerStopFallsBackToKill(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "ignore-stop")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}

	start := time.Now()
	if err := r.Stop(ctx, srv.ID, 300*time.Millisecond); err != nil {
		t.Fatalf("stopping server: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 300*time.Millisecond {
		t.Errorf("Stop returned after %v, before the timeout elapsed", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Stop took %v, far longer than the timeout", elapsed)
	}

	status, err := r.Status(ctx, srv.ID)
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status.isActive() {
		t.Fatalf("status after forced stop = %q, want a terminal state", status)
	}
}

func TestProcessRunnerCrashIsReported(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "crash")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := r.Status(ctx, srv.ID)
		if err != nil {
			t.Fatalf("reading status: %v", err)
		}
		if status == StatusCrashed {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server that exited non-zero was never reported as crashed")
}

func TestProcessRunnerDoubleStartRejected(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() { _ = r.Kill(context.Background(), srv.ID) })

	if err := r.Start(ctx, srv); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start = %v, want ErrAlreadyRunning", err)
	}
}

func TestProcessRunnerUnknownServer(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")

	if _, err := r.Status(ctx, "nope"); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("Status of unknown server = %v, want ErrServerNotFound", err)
	}
	if _, err := r.History(ctx, "nope", 10); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("History of unknown server = %v, want ErrServerNotFound", err)
	}
	if err := r.SendCommand(ctx, "nope", "list"); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("SendCommand to unknown server = %v, want ErrServerNotFound", err)
	}
}

// Stopping the server must close every console subscription so WebSocket
// handlers unblock instead of leaking.
func TestProcessRunnerStopClosesSubscriptions(t *testing.T) {
	ctx := context.Background()
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting server: %v", err)
	}

	ch, unsubscribe, err := r.Subscribe(ctx, srv.ID)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer unsubscribe()

	if err := r.Stop(ctx, srv.ID, 5*time.Second); err != nil {
		t.Fatalf("stopping server: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed, as required
			}
		case <-deadline:
			t.Fatal("console subscription was not closed when the server stopped")
		}
	}
}

func TestDefaultCommandBuilder(t *testing.T) {
	srv := &Server{
		ID:       "01TEST",
		Dir:      t.TempDir(),
		RAMMb:    2048,
		JarName:  "server.jar",
		JavaArgs: []string{"-XX:+UseG1GC"},
	}

	name, args, err := DefaultCommandBuilder(srv)
	if err != nil {
		t.Fatalf("DefaultCommandBuilder: %v", err)
	}
	if name != "java" {
		t.Errorf("executable = %q, want java", name)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{"-Xms2048M", "-Xmx2048M", "-XX:+UseG1GC", "-jar server.jar", "nogui"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}

	if _, _, err := DefaultCommandBuilder(&Server{ID: "x"}); err == nil {
		t.Error("DefaultCommandBuilder without a jar returned no error")
	}
}
