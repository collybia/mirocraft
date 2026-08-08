package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The image the fake server runs in. Small, always available, and the shell it
// carries is enough to stand in for a JVM that reads commands and prints them.
const testImage = "alpine:3.20"

// requireDocker skips when there is no daemon to talk to.
//
// Skipping rather than failing: a host without Docker is a supported install,
// so a developer on one must still be able to run the suite. CI with Docker
// present runs these for real.
func requireDocker(t *testing.T) *DockerRunner {
	t.Helper()

	r, err := NewDockerRunner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Available(ctx); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}

	// The fake server replaces the java invocation entirely, so no image with
	// a JVM is pulled and no jar has to exist.
	r.Image = func(int) string { return testImage }
	r.StopCommand = "stop"

	ensureTestImage(t, r)
	return r
}

func ensureTestImage(t *testing.T, r *DockerRunner) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	exists, err := r.Client().ImageExists(ctx, testImage)
	if err != nil {
		t.Skipf("checking for %s: %v", testImage, err)
	}
	if exists {
		return
	}
	if err := r.Client().PullImage(ctx, testImage, nil); err != nil {
		t.Skipf("could not pull %s: %v", testImage, err)
	}
}

// dockerTestServer returns a server whose "java" is a shell loop: it announces
// itself, echoes what it is told and exits on the stop command, which is the
// shape of a Minecraft server as far as the runner is concerned.
func dockerTestServer(t *testing.T, r *DockerRunner, id string) *Server {
	t.Helper()

	dir := t.TempDir()
	// Something to prove the bind mount landed.
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	r.Image = func(int) string { return testImage }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = r.Kill(ctx, id)
		_ = r.Client().RemoveContainer(ctx, ContainerPrefix+id, true)
	})

	return &Server{ID: id, Name: "docker-test", Dir: dir, RAMMb: 256}
}

func TestDockerRunnerLifecycle(t *testing.T) {
	r := requireDocker(t)
	srv := dockerTestServer(t, r, "01DOCKERLIFE")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	startShellServer(t, r, srv)

	waitForDockerLine(t, r, srv.ID, "ready")

	// The bind mount: the server can see its own directory.
	if err := r.SendCommand(ctx, srv.ID, "cat /data/marker.txt"); err != nil {
		t.Fatalf("sending a command: %v", err)
	}
	waitForDockerLine(t, r, srv.ID, "echo: cat /data/marker.txt")

	status, err := r.Status(ctx, srv.ID)
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != StatusRunning {
		t.Errorf("status = %q, want %q", status, StatusRunning)
	}

	stats, err := r.Stats(ctx, srv.ID)
	if err != nil {
		t.Fatalf("reading stats: %v", err)
	}
	if stats.Uptime <= 0 {
		t.Errorf("uptime = %v", stats.Uptime)
	}
	if stats.RAMBytes == 0 {
		t.Error("no memory reported for a running container")
	}

	// Starting a running server must be refused rather than making a second
	// container fight over the same directory.
	if err := r.Start(ctx, srv); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("starting twice: %v, want ErrAlreadyRunning", err)
	}

	if err := r.Stop(ctx, srv.ID, 30*time.Second); err != nil {
		t.Fatalf("stopping: %v", err)
	}

	status, err = r.Status(ctx, srv.ID)
	if err != nil {
		t.Fatalf("reading status after the stop: %v", err)
	}
	if status != StatusStopped {
		t.Errorf("status = %q after a graceful stop, want %q — it must not read as a crash",
			status, StatusStopped)
	}
}

// A server that ignores the stop command is killed after the timeout.
func TestDockerRunnerKillsAStubbornServer(t *testing.T) {
	r := requireDocker(t)
	srv := dockerTestServer(t, r, "01DOCKERSTUBBORN")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Sleeps forever and never reads stdin.
	startRawServer(t, r, srv, []string{"sh", "-c", `echo ready; sleep 3600`})
	waitForDockerLine(t, r, srv.ID, "ready")

	start := time.Now()
	if err := r.Stop(ctx, srv.ID, 2*time.Second); err != nil {
		t.Fatalf("stopping: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Fatalf("the stop took %s", elapsed)
	}

	status, _ := r.Status(ctx, srv.ID)
	if status.IsActive() {
		t.Fatalf("status = %q after the kill", status)
	}
}

// A container that exits by itself with a non-zero code is a crash, and the
// panel has to say so rather than report a clean stop.
func TestDockerRunnerReportsACrash(t *testing.T) {
	r := requireDocker(t)
	srv := dockerTestServer(t, r, "01DOCKERCRASH")

	startRawServer(t, r, srv, []string{"sh", "-c", `echo "starting"; exit 3`})

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		status, err := r.Status(context.Background(), srv.ID)
		if err != nil {
			t.Fatalf("reading status: %v", err)
		}
		if !status.IsActive() {
			if status != StatusCrashed {
				t.Fatalf("status = %q, want %q", status, StatusCrashed)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the container never settled")
}

// A container outlives the daemon that made it, so a restarted daemon must
// find the servers it left running rather than assume they are gone.
func TestDockerRunnerAdoptsARunningContainer(t *testing.T) {
	first := requireDocker(t)
	srv := dockerTestServer(t, first, "01DOCKERADOPT")

	startRawServer(t, first, srv, []string{"sh", "-c",
		`echo ready; while read line; do echo "echo: $line"; done`})
	waitForDockerLine(t, first, srv.ID, "ready")

	// A second runner stands in for a restarted daemon: it has never heard of
	// this server.
	second, err := NewDockerRunner("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("building the second runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := second.Status(ctx, srv.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("before adopting: %v, want ErrServerNotFound", err)
	}
	if err := second.Adopt(ctx); err != nil {
		t.Fatalf("adopting: %v", err)
	}

	status, err := second.Status(ctx, srv.ID)
	if err != nil {
		t.Fatalf("after adopting: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q after adoption, want %q", status, StatusRunning)
	}

	// And the adopted console is live, not just a status.
	if err := second.SendCommand(ctx, srv.ID, "still here"); err != nil {
		t.Fatalf("sending to an adopted server: %v", err)
	}
	waitForDockerLine(t, second, srv.ID, "echo: still here")

	// Among, not equal to: the host may well be running other Mirocraft
	// servers, and whether it is is none of this test's business.
	if !slices.Contains(second.RunningServers(), srv.ID) {
		t.Errorf("RunningServers = %v, want it to include %s", second.RunningServers(), srv.ID)
	}

	// A server up for a week must not report zero uptime just because the
	// daemon has only known it since its own restart.
	stats, err := second.Stats(ctx, srv.ID)
	if err != nil {
		t.Fatalf("reading the stats of an adopted server: %v", err)
	}
	if stats.Uptime <= 0 || stats.StartedAt.IsZero() {
		t.Errorf("adopted uptime = %v (started %v), want it taken from the container",
			stats.Uptime, stats.StartedAt)
	}

	if err := second.Kill(ctx, srv.ID); err != nil {
		t.Errorf("killing the adopted server: %v", err)
	}
}

func TestDockerRunnerUnknownServer(t *testing.T) {
	r := requireDocker(t)
	ctx := context.Background()

	for name, err := range map[string]error{
		"status":  mustErr(func() error { _, e := r.Status(ctx, "01NOPE"); return e }),
		"stats":   mustErr(func() error { _, e := r.Stats(ctx, "01NOPE"); return e }),
		"history": mustErr(func() error { _, e := r.History(ctx, "01NOPE", 10); return e }),
		"stop":    r.Stop(ctx, "01NOPE", time.Second),
		"kill":    r.Kill(ctx, "01NOPE"),
		"command": r.SendCommand(ctx, "01NOPE", "say hi"),
	} {
		if !errors.Is(err, ErrServerNotFound) {
			t.Errorf("%s on an unknown server: %v, want ErrServerNotFound", name, err)
		}
	}
}

func TestDefaultImage(t *testing.T) {
	cases := map[int]string{
		8:  "eclipse-temurin:8-jre",
		17: "eclipse-temurin:17-jre",
		21: "eclipse-temurin:21-jre",
		25: "eclipse-temurin:25-jre",
		// A server whose Java version could not be determined still has to
		// start, so zero picks a sensible current default rather than
		// producing "eclipse-temurin:0-jre".
		0: "eclipse-temurin:21-jre",
	}
	for major, want := range cases {
		if got := DefaultImage(major); got != want {
			t.Errorf("DefaultImage(%d) = %q, want %q", major, got, want)
		}
	}
}

func TestContainerCommand(t *testing.T) {
	cmd := containerCommand(&Server{RAMMb: 2048, JarName: "server.jar", JavaArgs: []string{"-XX:+UseG1GC"}})

	joined := strings.Join(cmd, " ")
	for _, want := range []string{"java", "-Xms2048M", "-Xmx2048M", "-XX:+UseG1GC", "-jar server.jar nogui"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command %q is missing %q", joined, want)
		}
	}
	// No path to a runtime: the image provides java on PATH, which is the
	// point of using an image.
	if cmd[0] != "java" {
		t.Errorf("command starts with %q, want the image's own java", cmd[0])
	}
}

// --- helpers ---

func mustErr(f func() error) error { return f() }

// startShellServer starts the fake server through the runner's own Start, with
// the java command swapped for a shell loop.
func startShellServer(t *testing.T, r *DockerRunner, srv *Server) {
	t.Helper()
	startRawServer(t, r, srv, []string{"sh", "-c",
		`echo ready; while read line; do echo "echo: $line"; done`})
}

// startRawServer starts a container with an explicit command.
//
// Start builds a java invocation, which alpine has no java for, so the command
// is overridden here. Everything else — naming, labels, binds, attach, the
// status machine — is the runner's own.
func startRawServer(t *testing.T, r *DockerRunner, srv *Server, cmd []string) {
	t.Helper()

	original := r.CommandFor
	r.CommandFor = func(*Server) []string { return cmd }
	t.Cleanup(func() { r.CommandFor = original })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := r.Start(ctx, srv); err != nil {
		t.Fatalf("starting: %v", err)
	}
}

func waitForDockerLine(t *testing.T, r *DockerRunner, serverID, needle string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		history, err := r.History(context.Background(), serverID, 200)
		if err == nil {
			for _, line := range history {
				if strings.Contains(line.Text, needle) {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	history, _ := r.History(context.Background(), serverID, 20)
	var seen []string
	for _, line := range history {
		seen = append(seen, line.Text)
	}
	t.Fatalf("the console never carried %q; it had:\n  %s", needle, strings.Join(seen, "\n  "))
}
