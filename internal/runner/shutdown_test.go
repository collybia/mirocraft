package runner

import (
	"context"
	"testing"
	"time"
)

// The daemon holds every server in a group that dies with it, so a daemon that
// exited without stopping them would take every world down with a kill — no
// "Saving worlds", whatever was in memory since the last autosave gone.
func TestShutdownStopsRunningServersGracefully(t *testing.T) {
	r := newTestRunner(t, "echo")

	first := &Server{ID: "01FIRST", Name: "first", Dir: t.TempDir()}
	second := &Server{ID: "01SECOND", Name: "second", Dir: t.TempDir()}

	for _, srv := range []*Server{first, second} {
		if err := r.Start(context.Background(), srv); err != nil {
			t.Fatalf("starting %s: %v", srv.ID, err)
		}
		waitForHistoryLine(t, r, srv.ID, "fake server starting")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("shutting down: %v", err)
	}

	for _, srv := range []*Server{first, second} {
		status, err := r.Status(context.Background(), srv.ID)
		if err != nil {
			t.Fatalf("reading the status of %s: %v", srv.ID, err)
		}
		if status != StatusStopped {
			t.Errorf("%s is %q after shutdown, want %q — it was killed rather than asked to stop",
				srv.ID, status, StatusStopped)
		}
	}
}

// A server that ignores the stop command must not hold the shutdown open past
// the timeout: the daemon is on a clock of its own.
func TestShutdownDoesNotWaitForeverOnAStubbornServer(t *testing.T) {
	r := newTestRunner(t, "ignore-stop")
	r.ShutdownTimeout = 300 * time.Millisecond

	srv := testServer(t)
	if err := r.Start(context.Background(), srv); err != nil {
		t.Fatalf("starting: %v", err)
	}
	waitForHistoryLine(t, r, srv.ID, "fake server starting")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("shutting down: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("shutdown took %s on a server that ignores stop", elapsed)
	}
	status, err := r.Status(context.Background(), srv.ID)
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status.IsActive() {
		t.Fatalf("the server is still %q after shutdown", status)
	}
}

// Shutting down with nothing running is the ordinary case on a fresh daemon.
func TestShutdownWithNoServersIsHarmless(t *testing.T) {
	r := newTestRunner(t, "echo")

	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutting down an idle runner: %v", err)
	}
}
