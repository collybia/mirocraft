package runner

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	psutil "github.com/shirou/gopsutil/v4/process"
)

var childPidLine = regexp.MustCompile(`child pid (\d+)`)

// A Minecraft server started through a wrapper, or a modded server that forks
// a helper, leaves children behind when only the direct child is signalled.
// Those children keep the world files open and the port bound, so the next
// start fails with "address already in use" for reasons an operator cannot
// see from the panel.
func TestKillReachesTheWholeTree(t *testing.T) {
	r := newTestRunner(t, "spawn-child")
	srv := testServer(t)

	if err := r.Start(context.Background(), srv); err != nil {
		t.Fatalf("starting: %v", err)
	}

	childPID := waitForChildPID(t, r, srv.ID)
	if !pidAlive(t, childPID) {
		t.Fatalf("the child %d was not running to begin with", childPID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Kill(ctx, srv.ID); err != nil {
		t.Fatalf("killing: %v", err)
	}

	// Termination is not instantaneous on either platform, so the check is a
	// short poll rather than a single look.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(t, childPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Do not leave a stray process behind if the assertion fails.
	if proc, err := psutil.NewProcess(int32(childPID)); err == nil {
		_ = proc.Kill()
	}
	t.Fatalf("the child %d outlived the kill, so a server's helpers would keep the port bound", childPID)
}

// The same, through the graceful path: a server that ignores "stop" is killed
// after the timeout, and that kill has to reach the tree too.
func TestStopTimeoutAlsoReachesTheWholeTree(t *testing.T) {
	r := newTestRunner(t, "spawn-child")
	srv := testServer(t)

	if err := r.Start(context.Background(), srv); err != nil {
		t.Fatalf("starting: %v", err)
	}

	childPID := waitForChildPID(t, r, srv.ID)

	// The fake server never reads stdin, so the stop command goes nowhere and
	// the timeout is what ends it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Stop(ctx, srv.ID, 200*time.Millisecond); err != nil {
		t.Fatalf("stopping: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(t, childPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	if proc, err := psutil.NewProcess(int32(childPID)); err == nil {
		_ = proc.Kill()
	}
	t.Fatalf("the child %d outlived the stop timeout", childPID)
}

// Grouping must not change what a normal, well-behaved server does.
func TestGroupingDoesNotDisturbAGracefulStop(t *testing.T) {
	r := newTestRunner(t, "echo")
	srv := testServer(t)

	if err := r.Start(context.Background(), srv); err != nil {
		t.Fatalf("starting: %v", err)
	}
	waitForHistoryLine(t, r, srv.ID, "fake server starting")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Stop(ctx, srv.ID, 10*time.Second); err != nil {
		t.Fatalf("stopping: %v", err)
	}

	status, err := r.Status(context.Background(), srv.ID)
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != StatusStopped {
		t.Fatalf("status = %q, want %q — a graceful stop must not read as a crash", status, StatusStopped)
	}
}

// --- helpers ---

func waitForChildPID(t *testing.T, r *ProcessRunner, serverID string) int {
	t.Helper()

	line := waitForHistoryLine(t, r, serverID, "child pid")
	match := childPidLine.FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("could not read a pid out of %q", line)
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parsing the child pid: %v", err)
	}
	return pid
}

// waitForHistoryLine polls the scrollback rather than a subscription, so it
// cannot miss a line published before the caller subscribed.
func waitForHistoryLine(t *testing.T, r *ProcessRunner, serverID, needle string) string {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		history, err := r.History(context.Background(), serverID, 100)
		if err == nil {
			for _, line := range history {
				if strings.Contains(line.Text, needle) {
					return line.Text
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the console never carried %q", needle)
	return ""
}

// pidAlive reports whether a process with this id is running.
//
// A pid can in principle be reused, which would make this answer "alive" about
// a different process. Within the second or two these tests take that is not a
// realistic outcome, and the alternative — matching on start time as well —
// would add a platform-specific comparison to a test whose subject is exactly
// the platform difference being hidden.
// pidAlive reports whether a process is still running, counting a zombie as
// gone.
//
// A zombie is a process that has already died and whose exit status nobody has
// collected: its entry in /proc outlives it. Killing the whole group kills the
// child's parent too, so there is nobody left to reap it until init gets
// round to it — and on a machine where init is slow about that, or is a
// container's PID 1 that does not reap at all, "the entry still exists" would
// read as "the kill did not work". It did; that is what a zombie means.
func pidAlive(t *testing.T, pid int) bool {
	t.Helper()

	exists, err := psutil.PidExists(int32(pid))
	if err != nil {
		t.Fatalf("checking pid %d: %v", pid, err)
	}
	if !exists {
		return false
	}

	proc, err := psutil.NewProcess(int32(pid))
	if err != nil {
		// Gone between the two calls, which is the answer.
		return false
	}
	statuses, err := proc.Status()
	if err != nil {
		// Windows reports no status this way and has no zombies: there the
		// entry existing is the whole answer.
		return true
	}
	for _, status := range statuses {
		if status == psutil.Zombie {
			return false
		}
	}
	return true
}
