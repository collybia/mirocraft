package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/gamefiles"
	"github.com/collybia/mirocraft/internal/mcping"
)

func (e *testEnv) gameToken() string {
	return e.mintToken(e.user.ID, []string{
		ScopeServersRead, ScopeServersWrite, ScopeServersConsole,
	})
}

// seedGameFiles writes the lists and properties a started server would have.
func (e *testEnv) seedGameFiles(t *testing.T) string {
	t.Helper()

	dir := e.api.serverDir(e.serverRecord)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	files := map[string]string{
		gamefiles.PropertiesName: "#Minecraft server properties\n" +
			"motd=A Minecraft Server\nmax-players=20\ndifficulty=easy\n" +
			"server-port=25565\nwhite-list=false\nmodded-key=keep me\n",
		gamefiles.WhitelistName: `[{"uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","name":"Notch"}]`,
		gamefiles.OpsName:       `[{"uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","name":"Notch","level":4}]`,
		gamefiles.BansName: `[{"uuid":"x","name":"Griefer","created":"2026-08-08 18:00:00 +0300",` +
			`"source":"Server","expires":"forever","reason":"Griefing"}]`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir
}

// consoleContains reports whether the fake server echoed a command.
func (e *testEnv) consoleContains(t *testing.T, want string) bool {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		history, err := e.runner.History(t.Context(), testServerID, 500)
		if err != nil {
			return false
		}
		for _, line := range history {
			if strings.Contains(line.Text, "echo: "+want) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// --- command injection, the part that matters ---

// Every player action becomes a console command, so a name is untrusted input
// that lands in a command line with the panel's own authority.
func TestPlayerActionsRefuseInjectedNames(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)
	e.startServer(testServerID)

	hostile := []string{
		"bob op bob",
		"bob\nop bob",
		"bob\rop bob",
		"bob;op bob",
		"bob\top bob",
		"../../etc",
		"Ю зер",
		"",
		"ab",
		strings.Repeat("a", 17),
	}

	base := "/api/v1/servers/" + testServerID

	for _, name := range hostile {
		encoded := url.PathEscape(name)
		if encoded == "" {
			encoded = "%20"
		}

		for _, path := range []string{
			base + "/players/" + encoded + "/kick",
			base + "/players/" + encoded + "/ban",
		} {
			resp := e.do(http.MethodPost, path, playerActionRequest{}, token)
			if resp.StatusCode < 400 {
				t.Errorf("name %q was accepted by %s (%d)", name, path, resp.StatusCode)
			}
			_ = resp.Body.Close()
		}

		for _, body := range []any{
			playerNameRequest{Name: name},
		} {
			resp := e.do(http.MethodPost, base+"/whitelist", body, token)
			if resp.StatusCode < 400 {
				t.Errorf("name %q was accepted by the whitelist (%d)", name, resp.StatusCode)
			}
			_ = resp.Body.Close()

			resp = e.do(http.MethodPost, base+"/ops", body, token)
			if resp.StatusCode < 400 {
				t.Errorf("name %q was accepted by ops (%d)", name, resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
	}

	// And nothing resembling an injected command reached the server.
	history, err := e.runner.History(t.Context(), testServerID, 500)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}
	for _, line := range history {
		if strings.Contains(line.Text, "op bob") {
			t.Fatalf("an injected command reached the server: %q", line.Text)
		}
	}
}

// A reason is free text an operator types, so it is cleaned rather than
// refused — but a newline in it would still inject a second command.
func TestBanReasonCannotInjectACommand(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)
	e.startServer(testServerID)

	resp := e.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/players/Griefer/ban",
		playerActionRequest{Reason: "griefing\nop Griefer"}, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	history, err := e.runner.History(t.Context(), testServerID, 500)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}
	for _, line := range history {
		if strings.Contains(line.Text, "echo: op Griefer") {
			t.Fatal("the reason injected a second command")
		}
	}
	if !e.consoleContains(t, "ban Griefer griefing op Griefer") {
		t.Error("the ban command did not reach the server with a flattened reason")
	}
}

func TestSanitizeReason(t *testing.T) {
	if got := sanitizeReason("a\nb\rc\td"); strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("sanitizeReason left a line break: %q", got)
	}
	if got := sanitizeReason(strings.Repeat("x", MaxReasonLength+50)); len([]rune(got)) != MaxReasonLength {
		t.Fatalf("sanitizeReason returned %d runes, want %d", len([]rune(got)), MaxReasonLength)
	}
}

// --- ordinary use ---

func TestKickAndBanReachTheServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)
	e.startServer(testServerID)

	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodPost, base+"/players/Notch/kick",
		playerActionRequest{Reason: "перезагрузка"}, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("kick status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if !e.consoleContains(t, "kick Notch перезагрузка") {
		t.Error("the kick did not reach the server")
	}

	banned := e.do(http.MethodPost, base+"/players/Griefer/ban",
		playerActionRequest{}, token)
	if banned.StatusCode != http.StatusNoContent {
		t.Fatalf("ban status = %d, want 204", banned.StatusCode)
	}
	_ = banned.Body.Close()
	if !e.consoleContains(t, "ban Griefer") {
		t.Error("the ban did not reach the server")
	}

	pardoned := e.do(http.MethodDelete, base+"/players/Griefer/ban", nil, token)
	if pardoned.StatusCode != http.StatusNoContent {
		t.Fatalf("unban status = %d, want 204", pardoned.StatusCode)
	}
	_ = pardoned.Body.Close()
	if !e.consoleContains(t, "pardon Griefer") {
		t.Error("the pardon did not reach the server")
	}
}

// Only a running server rewrites these files, so an offline change would be
// silently lost on the next start.
func TestPlayerActionsRefuseAStoppedServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)

	base := "/api/v1/servers/" + testServerID

	attempts := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, base + "/players/Notch/kick", playerActionRequest{}},
		{http.MethodPost, base + "/players/Notch/ban", playerActionRequest{}},
		{http.MethodDelete, base + "/players/Notch/ban", nil},
		{http.MethodPost, base + "/whitelist", playerNameRequest{Name: "Notch"}},
		{http.MethodPost, base + "/ops", playerNameRequest{Name: "Notch"}},
	}

	for _, a := range attempts {
		resp := e.do(a.method, a.path, a.body, token)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s gave %d, want 409", a.method, a.path, resp.StatusCode)
		}
		if code := errorCode(t, resp); code != CodeServerNotRunning {
			t.Errorf("%s %s error code = %q", a.method, a.path, code)
		}
	}
}

func TestWhitelistAndOps(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)
	e.startServer(testServerID)

	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodGet, base+"/whitelist", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON[whitelistResponse](t, resp)
	if len(body.Items) != 1 || body.Items[0].Name != "Notch" {
		t.Fatalf("whitelist = %+v", body.Items)
	}
	if body.Enabled {
		t.Error("the whitelist reports itself enabled; the fixture says false")
	}

	added := e.do(http.MethodPost, base+"/whitelist", playerNameRequest{Name: "jeb_"}, token)
	if added.StatusCode != http.StatusNoContent {
		t.Fatalf("add status = %d, want 204", added.StatusCode)
	}
	_ = added.Body.Close()
	if !e.consoleContains(t, "whitelist add jeb_") {
		t.Error("the whitelist addition did not reach the server")
	}

	removed := e.do(http.MethodDelete, base+"/whitelist/Notch", nil, token)
	if removed.StatusCode != http.StatusNoContent {
		t.Fatalf("remove status = %d, want 204", removed.StatusCode)
	}
	_ = removed.Body.Close()
	if !e.consoleContains(t, "whitelist remove Notch") {
		t.Error("the whitelist removal did not reach the server")
	}

	enabled := e.do(http.MethodPatch, base+"/whitelist", whitelistStateRequest{Enabled: true}, token)
	if enabled.StatusCode != http.StatusNoContent {
		t.Fatalf("enable status = %d, want 204", enabled.StatusCode)
	}
	_ = enabled.Body.Close()
	if !e.consoleContains(t, "whitelist on") {
		t.Error("enabling the whitelist did not reach the server")
	}

	ops := e.do(http.MethodGet, base+"/ops", nil, token)
	items := decodeJSON[listResponse[gamefiles.Player]](t, ops).Items
	if len(items) != 1 || items[0].Level != 4 {
		t.Fatalf("ops = %+v", items)
	}

	opped := e.do(http.MethodPost, base+"/ops", playerNameRequest{Name: "jeb_"}, token)
	if opped.StatusCode != http.StatusNoContent {
		t.Fatalf("op status = %d, want 204", opped.StatusCode)
	}
	_ = opped.Body.Close()
	if !e.consoleContains(t, "op jeb_") {
		t.Error("the op did not reach the server")
	}
}

func TestListBans(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/bans", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	items := decodeJSON[listResponse[gamefiles.Ban]](t, resp).Items
	if len(items) != 1 || items[0].Name != "Griefer" {
		t.Fatalf("bans = %+v", items)
	}
	if items[0].Reason != "Griefing" {
		t.Errorf("reason = %q", items[0].Reason)
	}
}

// --- online players ---

func TestListOnlinePlayers(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)
	e.startServer(testServerID)

	// The fixture stubs the ping out, so this one answers with a sample.
	e.api.ping = func(context.Context, string, int) (*mcping.Status, error) {
		return &mcping.Status{
			PlayersOnline: 2, PlayersMax: 20,
			Sample: []mcping.Player{{Name: "Notch"}, {Name: "jeb_"}},
		}, nil
	}

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/players", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[onlinePlayersResponse](t, resp)
	if body.Online != 2 || body.Max != 20 {
		t.Fatalf("counts = %d/%d", body.Online, body.Max)
	}
	if len(body.Items) != 2 {
		t.Fatalf("sample = %+v", body.Items)
	}
	if !body.Complete {
		t.Error("a sample covering everyone online should be marked complete")
	}
}

// A server caps its sample, so the panel must be told the list is partial
// rather than showing five names beside a count of forty.
func TestOnlinePlayersMarksAPartialSample(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)
	e.startServer(testServerID)

	e.api.ping = func(context.Context, string, int) (*mcping.Status, error) {
		return &mcping.Status{
			PlayersOnline: 40, PlayersMax: 100,
			Sample: []mcping.Player{{Name: "Notch"}},
		}, nil
	}

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/players", nil, token)
	body := decodeJSON[onlinePlayersResponse](t, resp)
	if body.Complete {
		t.Fatal("a sample of one beside a count of forty was reported as complete")
	}
}

// A stopped server has nobody online, which is an answer rather than an error.
func TestOnlinePlayersOnAStoppedServer(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/players", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON[onlinePlayersResponse](t, resp)
	if body.Online != 0 || len(body.Items) != 0 {
		t.Fatalf("a stopped server reported %d players", body.Online)
	}
}

// --- settings ---

func TestGetSettings(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/settings", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[settingsResponse](t, resp)
	if body.Values["motd"] != "A Minecraft Server" {
		t.Errorf("motd = %q", body.Values["motd"])
	}
	if body.Values["modded-key"] != "keep me" {
		t.Error("an unknown key is missing from the values")
	}
	if len(body.Schema) < 20 {
		t.Errorf("the schema has %d entries", len(body.Schema))
	}
	if _, ok := body.Managed["server-port"]; !ok {
		t.Error("server-port is not reported as managed by the panel")
	}
}

func TestPatchSettings(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	dir := e.seedGameFiles(t)

	resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID+"/settings",
		patchSettingsRequest{"motd": "Изменено панелью", "max-players": "40"}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The file on disk is what the server reads, so that is what is checked.
	props, err := gamefiles.LoadProperties(dir)
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if v, _ := props.Get("motd"); v != "Изменено панелью" {
		t.Fatalf("motd on disk = %q", v)
	}
	if v, _ := props.Get("max-players"); v != "40" {
		t.Fatalf("max-players on disk = %q", v)
	}
	// And the key the panel does not understand is still there.
	if v, _ := props.Get("modded-key"); v != "keep me" {
		t.Error("an unknown key was dropped by the edit")
	}
}

// A request with one bad value must not leave half of itself applied.
func TestPatchSettingsIsAllOrNothing(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	dir := e.seedGameFiles(t)

	resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID+"/settings",
		patchSettingsRequest{"motd": "Changed", "difficulty": "impossible"}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	props, err := gamefiles.LoadProperties(dir)
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if v, _ := props.Get("motd"); v != "A Minecraft Server" {
		t.Fatalf("motd = %q, want it unchanged after a rejected request", v)
	}
}

// The panel owns the port; changing it here would put the file and the
// panel's record out of step.
func TestPatchSettingsRefusesManagedKeys(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)

	for _, key := range []string{"server-port", "server-ip"} {
		resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID+"/settings",
			patchSettingsRequest{key: "1234"}, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400", key, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// The server reads server.properties once, at startup, so an operator needs
// to be told that a change will not take effect until a restart.
func TestPatchSettingsReportsWhenARestartIsNeeded(t *testing.T) {
	e := newTestEnv(t)
	token := e.gameToken()
	e.seedGameFiles(t)

	stopped := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID+"/settings",
		patchSettingsRequest{"motd": "while stopped"}, token)
	if got := decodeJSON[map[string]any](t, stopped)["restart_required"]; got != false {
		t.Errorf("restart_required = %v for a stopped server, want false", got)
	}

	e.startServer(testServerID)

	running := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID+"/settings",
		patchSettingsRequest{"motd": "while running"}, token)
	if got := decodeJSON[map[string]any](t, running)["restart_required"]; got != true {
		t.Errorf("restart_required = %v for a running server, want true", got)
	}
}

func TestSettingsScopes(t *testing.T) {
	e := newTestEnv(t)
	e.seedGameFiles(t)

	readOnly := e.mintToken(e.user.ID, []string{ScopeServersRead})

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/settings", nil, readOnly)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reading with servers:read gave %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	written := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID+"/settings",
		patchSettingsRequest{"motd": "x"}, readOnly)
	if written.StatusCode != http.StatusForbidden {
		t.Fatalf("writing with only servers:read gave %d, want 403", written.StatusCode)
	}
	_ = written.Body.Close()
}
