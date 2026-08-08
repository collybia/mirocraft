package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// dailyRestart is the chain the documentation uses as its example: warn the
// players, give them a moment, then restart.
func dailyRestart() []map[string]any {
	return []map[string]any{
		{"type": "command", "payload": map[string]any{"command": "say Рестарт через минуту"}},
		{"type": "wait", "payload": map[string]any{"seconds": 1}},
		{"type": "power", "payload": map[string]any{"action": "restart"}},
	}
}

func (e *testEnv) createSchedule(token string, body map[string]any) *http.Response {
	e.t.Helper()
	return e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/schedules", body, token)
}

func (e *testEnv) fullToken() string {
	e.t.Helper()
	return e.mintToken(e.user.ID, []string{
		ScopeServersRead, ScopeServersWrite, ScopeServersPower,
		ScopeServersConsole, ScopeBackupsWrite,
	})
}

// --- CRUD ---

func TestScheduleCreateAndList(t *testing.T) {
	env := newTestEnv(t)
	token := env.fullToken()

	resp := env.createSchedule(token, map[string]any{
		"name": "Ночной рестарт", "cron": "0 4 * * *", "actions": dailyRestart(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	created := decodeJSON[scheduleResponse](t, resp)
	if created.ID == "" {
		t.Fatal("the created schedule has no id")
	}
	if len(created.Actions) != 3 {
		t.Fatalf("actions = %+v", created.Actions)
	}
	if !created.Enabled {
		t.Error("a schedule was created disabled by default")
	}
	// An operator should not have to wait overnight to find out whether the
	// expression means what they think.
	if created.NextRunAt == nil || !created.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v", created.NextRunAt)
	}

	list := decodeJSON[listResponse[scheduleResponse]](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/schedules", nil, token))
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Items)
	}
}

func TestSchedulePatch(t *testing.T) {
	env := newTestEnv(t)
	token := env.fullToken()

	created := decodeJSON[scheduleResponse](t, env.createSchedule(token, map[string]any{
		"name": "Ночной рестарт", "cron": "0 4 * * *", "actions": dailyRestart(),
	}))

	resp := env.do(http.MethodPatch,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID,
		map[string]any{"enabled": false, "cron": "30 5 * * *"}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	patched := decodeJSON[scheduleResponse](t, resp)
	if patched.Enabled {
		t.Error("the schedule is still enabled")
	}
	if patched.Cron != "30 5 * * *" {
		t.Errorf("cron = %q", patched.Cron)
	}
	// A disabled schedule has no next run to show.
	if patched.NextRunAt != nil {
		t.Errorf("next_run_at = %v on a disabled schedule", patched.NextRunAt)
	}
	// Untouched fields survive a partial patch.
	if patched.Name != "Ночной рестарт" || len(patched.Actions) != 3 {
		t.Errorf("patch clobbered other fields: %+v", patched)
	}
}

func TestScheduleDelete(t *testing.T) {
	env := newTestEnv(t)
	token := env.fullToken()

	created := decodeJSON[scheduleResponse](t, env.createSchedule(token, map[string]any{
		"name": "Ночной рестарт", "cron": "0 4 * * *", "actions": dailyRestart(),
	}))

	resp := env.do(http.MethodDelete,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	list := decodeJSON[listResponse[scheduleResponse]](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/schedules", nil, token))
	if len(list.Items) != 0 {
		t.Fatalf("the schedule survived deletion")
	}
}

// A schedule id belonging to another server must not be reachable through a
// server the caller does own.
func TestScheduleIsScopedToItsServer(t *testing.T) {
	env := newTestEnv(t)
	token := env.fullToken()

	// A schedule on the other user's server, written straight to the store so
	// the test does not depend on that user's tokens.
	foreign := &store.Schedule{
		ServerID: otherServerID, Name: "чужое", Cron: "0 4 * * *",
		Actions: []store.Action{{Type: store.ActionBackup}}, Enabled: true,
	}
	if err := env.db.Schedules.Create(context.Background(), foreign); err != nil {
		t.Fatalf("creating the foreign schedule: %v", err)
	}

	for _, call := range []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/servers/" + testServerID + "/schedules/" + foreign.ID},
		{http.MethodDelete, "/api/v1/servers/" + testServerID + "/schedules/" + foreign.ID},
		{http.MethodPost, "/api/v1/servers/" + testServerID + "/schedules/" + foreign.ID + "/run"},
	} {
		resp := env.do(call.method, call.path, map[string]any{}, token)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", call.path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// And it is still there.
	if _, err := env.db.Schedules.GetByID(context.Background(), foreign.ID); err != nil {
		t.Fatalf("the foreign schedule is gone: %v", err)
	}
}

// --- validation ---

func TestScheduleValidation(t *testing.T) {
	env := newTestEnv(t)
	token := env.fullToken()

	tooMany := make([]map[string]any, MaxScheduleActions+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"type": "backup", "payload": map[string]any{}}
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no name", map[string]any{"cron": "0 4 * * *", "actions": dailyRestart()}},
		{"no cron", map[string]any{"name": "x", "actions": dailyRestart()}},
		// Accepted and then silently never firing is the worst outcome.
		{"a cron that cannot be parsed", map[string]any{"name": "x", "cron": "99 * * * *", "actions": dailyRestart()}},
		{"no actions", map[string]any{"name": "x", "cron": "0 4 * * *", "actions": []any{}}},
		{"too many actions", map[string]any{"name": "x", "cron": "0 4 * * *", "actions": tooMany}},
		{"an unknown action type", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "selfdestruct"}}}},
		{"an empty command", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "command", "payload": map[string]any{"command": "   "}}}}},
		// The console refuses control characters; a chain must not be a way
		// around that.
		{"a command with a newline", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "command", "payload": map[string]any{"command": "say hi\nop me"}}}}},
		{"an unknown power action", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "power", "payload": map[string]any{"action": "explode"}}}}},
		{"a wait with no seconds", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "wait", "payload": map[string]any{}}}}},
		{"a negative wait", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "wait", "payload": map[string]any{"seconds": -5}}}}},
		// A chain that could out-wait its own task deadline would be killed
		// halfway through.
		{"a wait past the limit", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{{"type": "wait", "payload": map[string]any{"seconds": MaxWaitSeconds + 1}}}}},
		{"waits that total past the limit", map[string]any{"name": "x", "cron": "0 4 * * *",
			"actions": []map[string]any{
				{"type": "wait", "payload": map[string]any{"seconds": MaxWaitSeconds}},
				{"type": "wait", "payload": map[string]any{"seconds": MaxWaitSeconds}},
			}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.createSchedule(token, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestScheduleLimitPerServer(t *testing.T) {
	env := newTestEnv(t)
	token := env.fullToken()

	body := map[string]any{"name": "x", "cron": "0 4 * * *",
		"actions": []map[string]any{{"type": "backup", "payload": map[string]any{}}}}

	for i := 0; i < MaxSchedulesPerServer; i++ {
		resp := env.createSchedule(token, body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("schedule %d: status %d", i, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	resp := env.createSchedule(token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 past the limit", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A schedule is a stored instruction to act later, so composing one must
// require what performing it by hand would. Otherwise servers:write alone
// would be a way to run arbitrary console commands.
func TestScheduleRequiresTheScopesItsActionsNeed(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name    string
		scopes  []string
		actions []map[string]any
	}{
		{"a command without console", []string{ScopeServersRead, ScopeServersWrite},
			[]map[string]any{{"type": "command", "payload": map[string]any{"command": "op Notch"}}}},
		{"power without power", []string{ScopeServersRead, ScopeServersWrite},
			[]map[string]any{{"type": "power", "payload": map[string]any{"action": "stop"}}}},
		{"a backup without backups:write", []string{ScopeServersRead, ScopeServersWrite},
			[]map[string]any{{"type": "backup", "payload": map[string]any{}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := env.mintToken(env.user.ID, tc.scopes)
			resp := env.createSchedule(token, map[string]any{
				"name": "x", "cron": "0 4 * * *", "actions": tc.actions,
			})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if code := errorCode(t, resp); code != CodeForbidden {
				t.Errorf("code = %q", code)
			}
		})
	}

	// Waiting needs nothing, so a chain of pure waits is allowed on the base
	// scopes.
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.createSchedule(token, map[string]any{
		"name": "x", "cron": "0 4 * * *",
		"actions": []map[string]any{{"type": "wait", "payload": map[string]any{"seconds": 1}}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a wait-only chain gave %d, want 201", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// Arming a chain somebody else composed is the same act as composing it.
func TestEnablingAChainRequiresItsScopes(t *testing.T) {
	env := newTestEnv(t)

	created := decodeJSON[scheduleResponse](t, env.createSchedule(env.fullToken(), map[string]any{
		"name": "x", "cron": "0 4 * * *", "enabled": false,
		"actions": []map[string]any{{"type": "command", "payload": map[string]any{"command": "op Notch"}}},
	}))

	weak := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPatch,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID,
		map[string]any{"enabled": true}, weak)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Disabling one, by contrast, takes nothing away from anybody.
	off := env.do(http.MethodPatch,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID,
		map[string]any{"enabled": false}, weak)
	if off.StatusCode != http.StatusOK {
		t.Fatalf("disabling gave %d, want 200", off.StatusCode)
	}
	_ = off.Body.Close()
}

// --- running ---

func TestRunScheduleSendsTheCommands(t *testing.T) {
	env := newTestEnv(t)
	env.startServer(testServerID)
	token := env.fullToken()

	created := decodeJSON[scheduleResponse](t, env.createSchedule(token, map[string]any{
		"name": "объявление", "cron": "0 4 * * *",
		"actions": []map[string]any{
			{"type": "command", "payload": map[string]any{"command": "say первое"}},
			{"type": "wait", "payload": map[string]any{"seconds": 1}},
			{"type": "command", "payload": map[string]any{"command": "say второе"}},
		},
	}))

	resp := env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID+"/run", nil, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	task := decodeJSON[taskAcceptedResponse](t, resp)

	// The fake server echoes what it is sent, so the console history is the
	// evidence the chain actually ran, in order.
	env.waitForConsole(t, "echo: say первое", 10*time.Second)
	env.waitForConsole(t, "echo: say второе", 10*time.Second)

	env.waitForTask(t, task.TaskID, TaskDone, 15*time.Second)

	// And the outcome is recorded where an operator can see it.
	stored, err := env.db.Schedules.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("reading the schedule back: %v", err)
	}
	if stored.LastStatus != store.ScheduleRunOK {
		t.Errorf("last_status = %q, error %q", stored.LastStatus, stored.LastError)
	}
	if stored.LastRunAt == nil {
		t.Error("last_run_at was not recorded")
	}
}

// "Warn the players, then stop the server" that failed to warn must not go on
// to stop it anyway.
func TestChainStopsAtTheFirstFailure(t *testing.T) {
	env := newTestEnv(t)
	// Deliberately not started: sending a command to a server that is not
	// running fails.
	token := env.fullToken()

	created := decodeJSON[scheduleResponse](t, env.createSchedule(token, map[string]any{
		"name": "x", "cron": "0 4 * * *",
		"actions": []map[string]any{
			{"type": "command", "payload": map[string]any{"command": "say никто не услышит"}},
			{"type": "backup", "payload": map[string]any{"note": "не должен появиться"}},
		},
	}))

	resp := env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID+"/run", nil, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	task := decodeJSON[taskAcceptedResponse](t, resp)

	env.waitForTask(t, task.TaskID, TaskFailed, 15*time.Second)

	backups, err := env.db.Backups.ListByServer(context.Background(), testServerID)
	if err != nil {
		t.Fatalf("listing backups: %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("the chain carried on after a failure and made %d backups", len(backups))
	}

	stored, err := env.db.Schedules.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("reading the schedule back: %v", err)
	}
	if stored.LastStatus != store.ScheduleRunFailed || stored.LastError == "" {
		t.Errorf("last_status = %q, last_error = %q", stored.LastStatus, stored.LastError)
	}
}

// A chain that waits can outlast the tick that started it, so it must not be
// started again while it is still going.
func TestChainDoesNotOverlapItself(t *testing.T) {
	env := newTestEnv(t)
	env.startServer(testServerID)
	token := env.fullToken()

	created := decodeJSON[scheduleResponse](t, env.createSchedule(token, map[string]any{
		"name": "долгая", "cron": "0 4 * * *",
		"actions": []map[string]any{{"type": "wait", "payload": map[string]any{"seconds": 5}}},
	}))

	first := env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID+"/run", nil, token)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first run: status %d", first.StatusCode)
	}
	_ = first.Body.Close()

	second := env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/schedules/"+created.ID+"/run", nil, token)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second run: status = %d, want 409", second.StatusCode)
	}
	_ = second.Body.Close()
}

// The tick fires a chain whose cron has come round, and only that one.
func TestFireDueSchedulesRespectsTheCron(t *testing.T) {
	env := newTestEnv(t)
	env.startServer(testServerID)
	ctx := context.Background()

	due := &store.Schedule{
		ServerID: testServerID, Name: "каждую минуту", Cron: "* * * * *", Enabled: true,
		Actions: []store.Action{{Type: store.ActionCommand,
			Payload: map[string]any{"command": "say сработало"}}},
	}
	if err := env.db.Schedules.Create(ctx, due); err != nil {
		t.Fatalf("creating the due schedule: %v", err)
	}

	notDue := &store.Schedule{
		ServerID: testServerID, Name: "раз в год", Cron: "0 0 1 1 *", Enabled: true,
		Actions: []store.Action{{Type: store.ActionCommand,
			Payload: map[string]any{"command": "say не должно сработать"}}},
	}
	if err := env.db.Schedules.Create(ctx, notDue); err != nil {
		t.Fatalf("creating the yearly schedule: %v", err)
	}

	disabled := &store.Schedule{
		ServerID: testServerID, Name: "выключено", Cron: "* * * * *", Enabled: false,
		Actions: []store.Action{{Type: store.ActionCommand,
			Payload: map[string]any{"command": "say выключено"}}},
	}
	if err := env.db.Schedules.Create(ctx, disabled); err != nil {
		t.Fatalf("creating the disabled schedule: %v", err)
	}

	// An hour on, so the every-minute chain is overdue and the yearly one is
	// not. Passed in rather than waiting for the clock.
	env.fireDue(t, time.Now().Add(time.Hour))

	env.waitForConsole(t, "echo: say сработало", 10*time.Second)

	history, err := env.runner.History(ctx, testServerID, 1000)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}
	for _, line := range history {
		if strings.Contains(line.Text, "не должно сработать") {
			t.Fatal("a schedule that was not due fired")
		}
		if strings.Contains(line.Text, "say выключено") {
			t.Fatal("a disabled schedule fired")
		}
	}
}

// A daemon that was down over a scheduled time should run the chain once on
// the next tick, not skip the day.
func TestAMissedScheduleRunsOnTheNextTick(t *testing.T) {
	env := newTestEnv(t)
	env.startServer(testServerID)
	ctx := context.Background()

	missed := &store.Schedule{
		ServerID: testServerID, Name: "пропущено", Cron: "0 4 * * *", Enabled: true,
		Actions: []store.Action{{Type: store.ActionCommand,
			Payload: map[string]any{"command": "say догнали"}}},
	}
	if err := env.db.Schedules.Create(ctx, missed); err != nil {
		t.Fatalf("creating the schedule: %v", err)
	}
	// It last ran two days ago, so its 04:00 has come and gone since.
	twoDaysAgo := time.Now().Add(-48 * time.Hour)
	if err := env.db.Schedules.MarkRun(ctx, missed.ID, twoDaysAgo); err != nil {
		t.Fatalf("marking the last run: %v", err)
	}

	env.fireDue(t, time.Now())
	env.waitForConsole(t, "echo: say догнали", 10*time.Second)
}

// --- helpers ---

// fireDue runs one scheduler tick with the clock the test chooses.
func (e *testEnv) fireDue(t *testing.T, now time.Time) {
	t.Helper()
	e.api.fireDueSchedules(context.Background(), now)
}

func (e *testEnv) waitForConsole(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		history, err := e.runner.History(context.Background(), testServerID, 1000)
		if err == nil {
			for _, line := range history {
				if strings.Contains(line.Text, needle) {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the console never carried %q", needle)
}

func (e *testEnv) waitForTask(t *testing.T, id, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last Task
	for time.Now().Before(deadline) {
		task, ok := e.api.tasks.get(id)
		if ok {
			last = task
			if task.Status == want {
				return
			}
			if task.Status == TaskDone || task.Status == TaskFailed {
				t.Fatalf("task settled as %q (%s), want %q", task.Status, task.Error, want)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s was %q after %s, want %q", id, last.Status, timeout, want)
}
