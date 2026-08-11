package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validCreate is a request that should always succeed, so tests can vary one
// field at a time.
func validCreate() createServerRequest {
	return createServerRequest{
		Name: "survival", Core: "paper", Version: "1.21.4",
		RAMMb: 2048, EULAAccepted: true,
	}
}

// writeToken mints a token with the scopes the server endpoints need.
func (e *testEnv) writeToken() string {
	return e.mintToken(e.user.ID, []string{
		ScopeServersRead, ScopeServersWrite, ScopeServersPower, ScopeServersConsole,
	})
}

func TestCreateServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	resp := e.do(http.MethodPost, "/api/v1/servers", validCreate(), token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	body := decodeJSON[serverResponse](t, resp)
	if body.ID == "" {
		t.Fatal("the created server has no id")
	}
	if body.Name != "survival" || body.Core != "paper" || body.RAMMb != 2048 {
		t.Fatalf("created server = %+v", body)
	}
	if body.OwnerID != e.user.ID {
		t.Errorf("owner = %q, want the caller %q", body.OwnerID, e.user.ID)
	}
	if body.Port == 0 {
		t.Error("no port was assigned")
	}

	stored, err := e.db.Servers.GetByID(t.Context(), body.ID)
	if err != nil {
		t.Fatalf("reading the stored server: %v", err)
	}

	// The directory and eula.txt must exist: recording the flag in the
	// database alone would not satisfy the server itself on first start.
	dir := e.api.serverDir(stored)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the server directory was not created: %v", err)
	}
	// Stored relative, so moving the data directory does not orphan it.
	if filepath.IsAbs(stored.Dir) {
		t.Errorf("the stored directory is absolute (%q); it must be relative to the data directory", stored.Dir)
	}
	eula, err := os.ReadFile(filepath.Join(dir, "eula.txt"))
	if err != nil {
		t.Fatalf("eula.txt was not written: %v", err)
	}
	if !strings.Contains(string(eula), "eula=true") {
		t.Errorf("eula.txt = %q, want eula=true", eula)
	}
}

// Creating a server without accepting the EULA must fail with 422, as
// documented.
func TestCreateServerRequiresEULA(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	req := validCreate()
	req.EULAAccepted = false

	resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeValidationFailed {
		t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
	}
}

// The panel is used in Russian and every example in its own documentation is
// Russian, but a server called "Выживание" was refused with a message saying
// only letters were allowed. The rule was justified by names becoming
// directory names, and they never did: a server's directory is its id.
func TestServerNamesAreNotLimitedToLatin(t *testing.T) {
	accepted := []string{
		"Выживание", "Тех-мир 2", "夜のサーバー", "Sürvival", "home", "7",
		"a b", "a-b", "a_b",
	}
	for _, name := range accepted {
		if got, err := normalizeServerName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		} else if got != name {
			t.Errorf("%q stored as %q", name, got)
		}
	}

	// What the rule is actually for. The last two are the interesting ones:
	// invisible runes let a name render as one thing and compare as another,
	// and a delete confirmation compares names.
	refused := []string{
		"", "   ", "../../etc", "a/b", `a\b`, "a:b", "a*b", "-lead", "trail-",
		"a\tb", "a\x00b", "emoji 🎮",
		"\u202eevil", // right-to-left override
		"a\u200bb",   // zero-width space
	}
	// Padding is trimmed rather than refused, on purpose: storing the raw
	// input would save a name that DELETE ?confirm= could never match.
	if got, err := normalizeServerName("  pad  "); err != nil || got != "pad" {
		t.Errorf(`normalizeServerName("  pad  ") = %q, %v`, got, err)
	}
	for _, name := range refused {
		if got, err := normalizeServerName(name); err == nil {
			t.Errorf("%q accepted as %q", name, got)
		}
	}
}

func TestCreateServerValidation(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	mutate := func(f func(*createServerRequest)) createServerRequest {
		req := validCreate()
		f(&req)
		return req
	}

	tests := []struct {
		name string
		req  createServerRequest
	}{
		{"no name", mutate(func(r *createServerRequest) { r.Name = "" })},
		{"path traversal in name", mutate(func(r *createServerRequest) { r.Name = "../../etc" })},
		{"slash in name", mutate(func(r *createServerRequest) { r.Name = "a/b" })},
		{"name too long", mutate(func(r *createServerRequest) { r.Name = strings.Repeat("a", 65) })},
		{"no core", mutate(func(r *createServerRequest) { r.Core = "" })},
		{"no version", mutate(func(r *createServerRequest) { r.Version = "" })},
		{"ram too small", mutate(func(r *createServerRequest) { r.RAMMb = 128 })},
		{"ram absurdly large", mutate(func(r *createServerRequest) { r.RAMMb = 1 << 30 })},
		{"unknown kind", mutate(func(r *createServerRequest) { r.Kind = "quantum" })},
		{"privileged port", mutate(func(r *createServerRequest) { r.Port = 80 })},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do(http.MethodPost, "/api/v1/servers", tc.req, token)
			if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 400 or 422", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestCreateServerRejectsTakenPort(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	req := validCreate()
	req.Port = 25565 // already used by the fixture server

	resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "port_in_use" {
		t.Fatalf("error code = %q, want port_in_use", code)
	}
}

// Automatic assignment must skip ports already handed out.
func TestCreateServerAllocatesFreePort(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	seen := map[int]bool{25565: true, 25566: true} // the two fixtures
	for i := 0; i < 3; i++ {
		req := validCreate()
		req.Name = "auto" + string(rune('a'+i))

		resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		port := decodeJSON[serverResponse](t, resp).Port

		if seen[port] {
			t.Fatalf("port %d was assigned twice", port)
		}
		seen[port] = true
	}
}

func TestCreateServerEnforcesServerLimit(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	// The fixture already owns one server, so a limit of 1 is reached.
	e.user.MaxServers = 1
	if err := e.db.Users.Update(t.Context(), e.user); err != nil {
		t.Fatalf("updating user: %v", err)
	}

	resp := e.do(http.MethodPost, "/api/v1/servers", validCreate(), token)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "insufficient_resources" {
		t.Fatalf("error code = %q, want insufficient_resources", code)
	}
}

func TestCreateServerEnforcesRAMLimit(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	// The fixture server already allocates 1024 MB.
	e.user.MaxRAMMb = 2048
	if err := e.db.Users.Update(t.Context(), e.user); err != nil {
		t.Fatalf("updating user: %v", err)
	}

	req := validCreate()
	req.RAMMb = 2048 // 1024 + 2048 exceeds the allowance

	resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "insufficient_resources" {
		t.Fatalf("error code = %q, want insufficient_resources", code)
	}
}

func TestCreateServerRequiresWriteScope(t *testing.T) {
	e := newTestEnv(t)

	// e.token holds only servers:read and servers:console.
	resp := e.do(http.MethodPost, "/api/v1/servers", validCreate(), e.token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// --- listing and reading ---

func TestListServersShowsOnlyYourOwn(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/servers", nil, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	items := decodeJSON[listResponse[serverResponse]](t, resp).Items
	if len(items) != 1 {
		t.Fatalf("listed %d servers, want only the caller's one", len(items))
	}
	if items[0].ID != testServerID {
		t.Fatalf("listed server %q, want %q", items[0].ID, testServerID)
	}
}

func TestListServersAdminSeesEverything(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/servers", nil, e.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	items := decodeJSON[listResponse[serverResponse]](t, resp).Items
	if len(items) != 2 {
		t.Fatalf("admin listed %d servers, want both fixtures", len(items))
	}
}

func TestListServersFilters(t *testing.T) {
	e := newTestEnv(t)

	byCore := e.do(http.MethodGet, "/api/v1/servers?core=paper", nil, e.token)
	if n := len(decodeJSON[listResponse[serverResponse]](t, byCore).Items); n != 1 {
		t.Fatalf("core filter returned %d servers, want 1", n)
	}

	none := e.do(http.MethodGet, "/api/v1/servers?core=forge", nil, e.token)
	if n := len(decodeJSON[listResponse[serverResponse]](t, none).Items); n != 0 {
		t.Fatalf("filtering on an unused core returned %d servers", n)
	}
}

func TestGetServer(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID, nil, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[serverResponse](t, resp)
	if body.ID != testServerID || body.Name != "owned" {
		t.Fatalf("server = %+v", body)
	}
	// Not running, so there is nothing to measure.
	if body.Metrics != nil {
		t.Errorf("a stopped server reported metrics: %+v", body.Metrics)
	}
}

func TestGetServerOfAnotherUserIsHidden(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+otherServerID, nil, e.token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// A running server must report uptime and memory.
func TestGetServerReportsMetricsWhenRunning(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID, nil, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[serverResponse](t, resp)
	if body.Status != "running" {
		t.Fatalf("status = %q, want running", body.Status)
	}
	if body.Metrics == nil {
		t.Fatal("a running server reported no metrics")
	}
	if body.Metrics.RAMUsedMb <= 0 {
		t.Errorf("ram_used_mb = %d, want a positive figure", body.Metrics.RAMUsedMb)
	}
	// The fake server is not a Minecraft server, so a list ping finds nothing
	// and the player counts stay absent rather than being reported as zero.
	if body.Metrics.PlayersOnline != nil {
		t.Errorf("players_online = %v, want absent for a server that does not speak the protocol",
			*body.Metrics.PlayersOnline)
	}
}

// --- patch ---

func TestPatchServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	name := "renamed"
	ram := 4096
	auto := true
	resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		patchServerRequest{Name: &name, RAMMb: &ram, AutoStart: &auto}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[serverResponse](t, resp)
	if body.Name != "renamed" || body.RAMMb != 4096 || !body.AutoStart {
		t.Fatalf("patched server = %+v", body)
	}
}

func TestPatchServerValidation(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	bad := "../escape"
	resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		patchServerRequest{Name: &bad}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// --- delete ---

// Deleting destroys worlds, so it must refuse without an explicit
// confirmation matching the server name.
func TestDeleteServerRequiresConfirmation(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	without := e.do(http.MethodDelete, "/api/v1/servers/"+testServerID, nil, token)
	if without.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete without confirmation gave %d, want 400", without.StatusCode)
	}
	_ = without.Body.Close()

	wrong := e.do(http.MethodDelete, "/api/v1/servers/"+testServerID+"?confirm=nonsense", nil, token)
	if wrong.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete with a wrong confirmation gave %d, want 400", wrong.StatusCode)
	}
	_ = wrong.Body.Close()

	if _, err := e.db.Servers.GetByID(t.Context(), testServerID); err != nil {
		t.Fatalf("the server was deleted despite the refusals: %v", err)
	}
}

func TestDeleteServerRemovesRecordAndFiles(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	created := e.do(http.MethodPost, "/api/v1/servers", validCreate(), token)
	server := decodeJSON[serverResponse](t, created)

	stored, err := e.db.Servers.GetByID(t.Context(), server.ID)
	if err != nil {
		t.Fatalf("reading the stored server: %v", err)
	}
	dir := e.api.serverDir(stored)

	resp := e.do(http.MethodDelete,
		"/api/v1/servers/"+server.ID+"?confirm="+server.Name, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := e.db.Servers.GetByID(t.Context(), server.ID); err == nil {
		t.Error("the server record still exists")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the server directory still exists: %v", err)
	}
}

// --- power ---

func TestPowerStartAndStop(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	start := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStart}, token)
	if start.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", start.StatusCode)
	}
	taskID := decodeJSON[taskAcceptedResponse](t, start).TaskID
	if taskID == "" {
		t.Fatal("start returned no task id")
	}
	t.Cleanup(func() { _ = e.runner.Kill(t.Context(), testServerID) })

	task := e.awaitTask(taskID, token)
	if task.Status != TaskDone {
		t.Fatalf("start task ended as %q: %s", task.Status, task.Error)
	}

	status, err := e.runner.Status(t.Context(), testServerID)
	if err != nil {
		t.Fatalf("reading runner status: %v", err)
	}
	if !status.IsActive() {
		t.Fatalf("the server is %q after a successful start task", status)
	}

	stop := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStop, TimeoutSeconds: 5}, token)
	if stop.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202", stop.StatusCode)
	}
	stopTask := e.awaitTask(decodeJSON[taskAcceptedResponse](t, stop).TaskID, token)
	if stopTask.Status != TaskDone {
		t.Fatalf("stop task ended as %q: %s", stopTask.Status, stopTask.Error)
	}
}

// awaitTask polls a task until it finishes.
func (e *testEnv) awaitTask(id, token string) Task {
	e.t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp := e.do(http.MethodGet, "/api/v1/tasks/"+id, nil, token)
		if resp.StatusCode != http.StatusOK {
			e.t.Fatalf("reading task %s gave %d", id, resp.StatusCode)
		}
		task := decodeJSON[Task](e.t, resp)
		if task.Status == TaskDone || task.Status == TaskFailed {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("task %s did not finish in time", id)
	return Task{}
}

func TestPowerRejectsStartingATwiceRunningServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()
	e.startServer(testServerID)

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStart}, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "server_already_running" {
		t.Fatalf("error code = %q, want server_already_running", code)
	}
}

func TestPowerRejectsStoppingAStoppedServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStop}, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeServerNotRunning {
		t.Fatalf("error code = %q, want %q", code, CodeServerNotRunning)
	}
}

func TestPowerRejectsUnknownAction(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: "explode"}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPowerRequiresPowerScope(t *testing.T) {
	e := newTestEnv(t)

	// e.token has no servers:power.
	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStart}, e.token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// --- tasks ---

func TestTaskOfAnotherUserIsHidden(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	start := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStart}, token)
	taskID := decodeJSON[taskAcceptedResponse](t, start).TaskID
	t.Cleanup(func() { _ = e.runner.Kill(t.Context(), testServerID) })

	// The task starts the process in the background; killing it before it has
	// spawned would leave the process alive and holding the temp directory.
	e.awaitTask(taskID, token)

	otherToken := e.mintToken(e.other.ID, []string{ScopeServersRead})
	resp := e.do(http.MethodGet, "/api/v1/tasks/"+taskID, nil, otherToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestListServerTasks(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	start := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/power",
		powerRequest{Action: ActionStart}, token)
	t.Cleanup(func() { _ = e.runner.Kill(t.Context(), testServerID) })
	e.awaitTask(decodeJSON[taskAcceptedResponse](t, start).TaskID, token)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/tasks", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	items := decodeJSON[listResponse[Task]](t, resp).Items
	if len(items) != 1 {
		t.Fatalf("listed %d tasks, want 1", len(items))
	}
	if items[0].Kind != "power.start" {
		t.Fatalf("task kind = %q, want power.start", items[0].Kind)
	}
}

func TestUnknownTaskIsNotFound(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/tasks/01NOSUCHTASK", nil, e.token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
