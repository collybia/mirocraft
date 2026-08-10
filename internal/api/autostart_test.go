package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/runner"
)

// A panel on a rented machine is restarted by things nobody chose. Without
// this the worlds stay down until somebody notices, and the switch that says
// otherwise is decoration.
func TestAutoStartBringsMarkedServersUp(t *testing.T) {
	env := newTestEnv(t)

	env.serverRecord.AutoStart = true
	if err := env.db.Servers.Update(context.Background(), env.serverRecord); err != nil {
		t.Fatalf("marking the server auto-start: %v", err)
	}

	env.api.StartAutoStartServers(context.Background())

	status, err := env.api.serverStatus(context.Background(), testServerID)
	if err != nil {
		t.Fatalf("reading the status: %v", err)
	}
	if !status.IsActive() {
		t.Fatalf("status = %q, want a running server", status)
	}
}

// The flag is off by default, and a daemon starting must not wake servers
// their owner left stopped on purpose.
func TestAutoStartLeavesUnmarkedServersAlone(t *testing.T) {
	env := newTestEnv(t)

	env.api.StartAutoStartServers(context.Background())

	if status, err := env.api.serverStatus(context.Background(), testServerID); err == nil && status.IsActive() {
		t.Fatalf("a server without auto_start was started anyway (%s)", status)
	}
}

// Restarting a server the operator stopped would make the stop button a
// suggestion, so only a crash counts.
func TestOnlyACrashTriggersAnAutoRestart(t *testing.T) {
	if crashed(runner.StatusStopped) {
		t.Error("a stopped server is treated as crashed")
	}
	if crashed(runner.StatusStopping) {
		t.Error("a stopping server is treated as crashed")
	}
	if !crashed(runner.StatusCrashed) {
		t.Error("a crashed server is not treated as crashed")
	}
}

// A crash loop is worse than a stopped server: it fills the disk with logs and
// buries the original fault under a thousand repetitions of it.
func TestAutoRestartGivesUpOnACrashLoop(t *testing.T) {
	now := time.Now()
	restarter := newAutoRestarter()
	restarter.now = func() time.Time { return now }

	for i := 1; i <= autoRestartAttempts; i++ {
		if !restarter.allow("s1") {
			t.Fatalf("attempt %d was refused, and it is within the budget", i)
		}
	}
	if restarter.allow("s1") {
		t.Error("a fourth attempt inside the window was allowed")
	}

	// Another server has its own budget.
	if !restarter.allow("s2") {
		t.Error("one server's crash loop stopped another from restarting")
	}

	// And a server that behaved for a while starts counting again: a crash
	// today has nothing to do with one last week.
	now = now.Add(autoRestartWindow + time.Minute)
	if !restarter.allow("s1") {
		t.Error("the window never reopened")
	}
}

// The bug this exists to prevent: an auto-restart goes through startServer
// too, so clearing the budget in there let a server crash forever, three at a
// time. Four kills in a row on a real machine produced four restarts.
func TestAnAutoRestartDoesNotClearItsOwnBudget(t *testing.T) {
	env := newTestEnv(t)

	for i := 0; i < autoRestartAttempts; i++ {
		env.api.restarts.allow(testServerID)
	}

	// The path an auto-restart takes, rather than the endpoint.
	if err := env.api.startServer(context.Background(), env.serverRecord); err != nil {
		t.Fatalf("starting: %v", err)
	}

	if env.api.restarts.allow(testServerID) {
		t.Error("the crash budget was cleared by a restart nobody asked for")
	}
}

// Starting a server by hand is a decision, and it should not be spent against
// the budget that exists to stop a loop.
func TestStartingByHandClearsTheCrashBudget(t *testing.T) {
	env := newTestEnv(t)

	for i := 0; i < autoRestartAttempts; i++ {
		env.api.restarts.allow(testServerID)
	}
	if env.api.restarts.allow(testServerID) {
		t.Fatal("the budget was not spent, so this test proves nothing")
	}

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		map[string]any{"action": "start"}, env.mintToken(env.user.ID, []string{ScopeServersPower}))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start gave %d", resp.StatusCode)
	}
	body := decodeJSON[taskAcceptedResponse](t, resp)
	env.waitForTask(t, body.TaskID, TaskDone, 30*time.Second)

	if !env.api.restarts.allow(testServerID) {
		t.Error("an operator's own start did not clear the budget")
	}
}
