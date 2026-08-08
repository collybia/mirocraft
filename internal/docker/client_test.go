package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// requireDaemon skips a test when there is no Docker to talk to.
//
// Skipping rather than failing: the panel is meant to work on hosts without
// Docker, so a developer on such a host must still be able to run the suite.
// CI that cares runs with Docker present and these stop being skipped.
func requireDaemon(t *testing.T) *Client {
	t.Helper()

	client, err := New("")
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Skipf("no docker daemon at %s: %v", client.Host(), err)
	}
	return client
}

func TestDemuxFrame(t *testing.T) {
	// Two frames, one per stream, exactly as the Engine writes them.
	var buf bytes.Buffer
	writeFrame(&buf, StreamStdout, "[INFO] Done (1.234s)!\n")
	writeFrame(&buf, StreamStderr, "[WARN] something\n")

	stream, payload, err := DemuxFrame(&buf)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if stream != StreamStdout || string(payload) != "[INFO] Done (1.234s)!\n" {
		t.Errorf("first frame = %d %q", stream, payload)
	}

	stream, payload, err = DemuxFrame(&buf)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if stream != StreamStderr || string(payload) != "[WARN] something\n" {
		t.Errorf("second frame = %d %q", stream, payload)
	}

	if _, _, err := DemuxFrame(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("after the last frame: %v, want EOF", err)
	}
}

// A desynchronised stream produces a length field that is not a length. The
// reader must refuse it rather than try to allocate it.
func TestDemuxFrameRefusesAnAbsurdLength(t *testing.T) {
	header := make([]byte, frameHeaderSize)
	header[0] = StreamStdout
	binary.BigEndian.PutUint32(header[4:], 1<<30)

	_, _, err := DemuxFrame(bytes.NewReader(header))
	if err == nil {
		t.Fatal("a 1 GiB frame was accepted")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v", err)
	}
}

// A truncated frame is an error, not a short read reported as success.
func TestDemuxFrameOnATruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, StreamStdout, "hello")
	truncated := buf.Bytes()[:frameHeaderSize+2]

	if _, _, err := DemuxFrame(bytes.NewReader(truncated)); err == nil {
		t.Fatal("a truncated frame was accepted")
	}
}

func writeFrame(w io.Writer, stream int, text string) {
	header := make([]byte, frameHeaderSize)
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	_, _ = w.Write(header)
	_, _ = io.WriteString(w, text)
}

func TestDialerRejectsNonsenseHosts(t *testing.T) {
	for _, host := range []string{"nonsense", "ftp://x", "://"} {
		if _, err := New(host); err == nil {
			t.Errorf("New(%q) was accepted", host)
		}
	}
}

// --- against a real daemon ---

func TestPingAndVersion(t *testing.T) {
	client := requireDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	version, err := client.Version(ctx)
	if err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version == "" {
		t.Fatal("the daemon reported no version")
	}
	t.Logf("docker %s at %s", version, client.Host())
}

func TestImageExistsIsFalseForNonsense(t *testing.T) {
	client := requireDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	exists, err := client.ImageExists(ctx, "mirocraft/definitely-not-an-image:v0")
	if err != nil {
		t.Fatalf("checking a missing image: %v", err)
	}
	if exists {
		t.Fatal("a made-up image reported as present")
	}
}

// The container lifecycle end to end: create, start, read the output, write to
// stdin, stop, remove.
func TestContainerLifecycle(t *testing.T) {
	client := requireDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const image = "alpine:3.20"
	ensureImage(t, ctx, client, image)

	// Echoes stdin back, which is what the console needs from a server.
	id, err := client.CreateContainer(ctx, ContainerSpec{
		Name:   "mirocraft-test-lifecycle",
		Image:  image,
		Cmd:    []string{"sh", "-c", `echo "ready"; while read line; do echo "echo: $line"; done`},
		Labels: map[string]string{"mirocraft.test": "1"},
	})
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	t.Cleanup(func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.RemoveContainer(removeCtx, id, true)
	})

	conn, err := client.Attach(ctx, id)
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := client.StartContainer(ctx, id); err != nil {
		t.Fatalf("starting: %v", err)
	}

	lines := make(chan string, 32)
	go func() {
		for {
			stream, payload, err := DemuxFrame(conn)
			if err != nil {
				close(lines)
				return
			}
			if stream == StreamStdout || stream == StreamStderr {
				for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
					lines <- line
				}
			}
		}
	}()

	waitFor(t, lines, "ready")

	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatalf("writing to stdin: %v", err)
	}
	waitFor(t, lines, "echo: hello")

	info, err := client.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if !info.State.Running {
		t.Fatalf("state = %+v, want running", info.State)
	}

	found, err := client.ListContainers(ctx, "mirocraft.test=1")
	if err != nil {
		t.Fatalf("listing by label: %v", err)
	}
	if len(found) == 0 {
		t.Error("the container was not found by its label, so a restarted daemon could not adopt it")
	}

	if err := client.StopContainer(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("stopping: %v", err)
	}

	info, err = client.InspectContainer(ctx, id)
	if err != nil {
		t.Fatalf("inspecting after the stop: %v", err)
	}
	if info.State.Running {
		t.Error("the container is still running after a stop")
	}
}

func TestInspectMissingContainerIsNotFound(t *testing.T) {
	client := requireDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := client.InspectContainer(ctx, "mirocraft-no-such-container")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound so callers can tell it apart", err)
	}
}

func ensureImage(t *testing.T, ctx context.Context, client *Client, ref string) {
	t.Helper()

	exists, err := client.ImageExists(ctx, ref)
	if err != nil {
		t.Fatalf("checking for %s: %v", ref, err)
	}
	if exists {
		return
	}
	if err := client.PullImage(ctx, ref, nil); err != nil {
		t.Skipf("could not pull %s (no network?): %v", ref, err)
	}
}

func waitFor(t *testing.T, lines chan string, want string) {
	t.Helper()

	deadline := time.After(60 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the stream ended while waiting for %q", want)
			}
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}
