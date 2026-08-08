package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/temertika/mirocraft/internal/runner"
	"github.com/temertika/mirocraft/internal/store"
)

const (
	testPassword  = "correct horse battery staple"
	testServerID  = "01TESTSERVER"
	otherServerID = "01OTHERSERVER"
	fakeServerEnv = "MIROCRAFT_FAKE_SERVER"
)

// The fake server echoes stdin to stdout, standing in for a Minecraft server.
// It is this test binary re-executed, which keeps the suite portable.
func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) != "" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeServer() {
	_, _ = io.WriteString(os.Stdout, "[INFO] fake server ready\n")

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				cmd := strings.TrimRight(string(buf[:i]), "\r")
				buf = buf[i+1:]
				if cmd == "stop" {
					os.Exit(0)
				}
				_, _ = io.WriteString(os.Stdout, "[INFO] echo: "+cmd+"\n")
			}
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

// testEnv is a running API backed by a real ProcessRunner driving a real
// child process — the console path end to end, with nothing stubbed.
type testEnv struct {
	t      *testing.T
	api    *API
	server *httptest.Server
	runner *runner.ProcessRunner
	db     *store.Store

	user  *store.User
	other *store.User
	admin *store.User

	token        string
	adminToken   string
	serverRecord *store.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}

	pr := runner.NewProcessRunner(slog.New(slog.NewTextHandler(io.Discard, nil)))
	pr.Build = func(*runner.Server) (string, []string, error) { return self, nil, nil }
	pr.Env = append(os.Environ(), fakeServerEnv+"=1")

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	a := New(Options{
		Store:   db,
		Console: pr,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// httptest sends no Origin header, so the default same-origin check
		// would reject the upgrade in tests.
		CheckOrigin: func(*http.Request) bool { return true },
	})

	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = pr.Shutdown(context.Background())
		_ = db.Close()
	})

	env := &testEnv{t: t, api: a, server: srv, runner: pr, db: db}
	env.seed()
	return env
}

// seed creates the fixture accounts and server records the tests share.
func (e *testEnv) seed() {
	e.t.Helper()
	ctx := context.Background()

	hash, err := store.HashPassword(testPassword)
	if err != nil {
		e.t.Fatalf("hashing password: %v", err)
	}

	e.user = &store.User{Email: "owner@example.com", PasswordHash: hash, Role: store.RoleUser}
	if err := e.db.Users.Create(ctx, e.user); err != nil {
		e.t.Fatalf("creating user: %v", err)
	}
	e.other = &store.User{Email: "other@example.com", PasswordHash: hash, Role: store.RoleUser}
	if err := e.db.Users.Create(ctx, e.other); err != nil {
		e.t.Fatalf("creating other user: %v", err)
	}
	e.admin = &store.User{Email: "admin@example.com", PasswordHash: hash, Role: store.RoleAdmin}
	if err := e.db.Users.Create(ctx, e.admin); err != nil {
		e.t.Fatalf("creating admin: %v", err)
	}

	e.token = e.mintToken(e.user.ID, []string{ScopeServersRead, ScopeServersConsole})
	e.adminToken = e.mintToken(e.admin.ID, AllScopes)

	e.serverRecord = &store.Server{
		ID: testServerID, OwnerID: e.user.ID, Name: "owned", Core: "paper",
		Version: "1.21.4", RAMMb: 1024, Port: 25565, Dir: e.t.TempDir(),
	}
	if err := e.db.Servers.Create(ctx, e.serverRecord); err != nil {
		e.t.Fatalf("creating server record: %v", err)
	}
	if err := e.db.Servers.Create(ctx, &store.Server{
		ID: otherServerID, OwnerID: e.other.ID, Name: "foreign", Core: "paper",
		Version: "1.21.4", RAMMb: 1024, Port: 25566, Dir: e.t.TempDir(),
	}); err != nil {
		e.t.Fatalf("creating other server record: %v", err)
	}
}

// mintToken creates an API token with the given scopes and returns its value.
func (e *testEnv) mintToken(userID string, scopes []string) string {
	e.t.Helper()

	value, hash, err := store.GenerateToken()
	if err != nil {
		e.t.Fatalf("generating token: %v", err)
	}
	err = e.db.Tokens.Create(context.Background(), &store.Token{
		UserID: userID, Name: "test", Hash: hash, Scopes: scopes, Kind: store.TokenKindAPI,
	})
	if err != nil {
		e.t.Fatalf("creating token: %v", err)
	}
	return value
}

// startServer launches the fake server process and waits until its first line
// is buffered, so tests do not race the process start.
func (e *testEnv) startServer(id string) {
	e.t.Helper()

	srv := &runner.Server{ID: id, Name: "test", Dir: e.t.TempDir()}
	if err := e.runner.Start(context.Background(), srv); err != nil {
		e.t.Fatalf("starting fake server: %v", err)
	}
	e.t.Cleanup(func() { _ = e.runner.Kill(context.Background(), id) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		history, err := e.runner.History(context.Background(), id, 10)
		if err == nil && len(history) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("fake server produced no output within the timeout")
}

func (e *testEnv) do(method, path string, body any, token string) *http.Response {
	e.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshalling request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatalf("building request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("performing request: %v", err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	return decodeJSON[errorResponse](t, resp).Error.Code
}

// --- auth ---

func TestConsoleEndpointsRequireAuth(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/servers/" + testServerID + "/console/history"},
		{http.MethodPost, "/api/v1/servers/" + testServerID + "/command"},
		{http.MethodPost, "/api/v1/servers/" + testServerID + "/console/ticket"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := e.do(tc.method, tc.path, nil, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if code := errorCode(t, resp); code != CodeUnauthorized {
				t.Fatalf("error code = %q, want %q", code, CodeUnauthorized)
			}
		})
	}
}

func TestConsoleRejectsBadToken(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/console/history", nil, "wrong-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestConsoleRequiresConsoleScope(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	// Deliberately without servers:console.
	readOnly := e.mintToken(e.user.ID, []string{ScopeServersRead})

	resp := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/console/history", nil, readOnly)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeForbidden {
		t.Fatalf("error code = %q, want %q", code, CodeForbidden)
	}
}

// A server owned by someone else must look like it does not exist, so the API
// does not leak which ids are real.
func TestConsoleHidesOtherUsersServers(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(otherServerID)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+otherServerID+"/console/history", nil, e.token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeServerNotFound {
		t.Fatalf("error code = %q, want %q", code, CodeServerNotFound)
	}
}

// --- history ---

func TestConsoleHistoryReturnsBufferedLines(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/console/history", nil, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[historyResponse](t, resp)
	if len(body.Items) == 0 {
		t.Fatal("history is empty, want the server's startup line")
	}
	first := body.Items[0]
	if first.Type != frameLine {
		t.Errorf("frame type = %q, want %q", first.Type, frameLine)
	}
	if first.Stream != runner.StreamStdout {
		t.Errorf("stream = %q, want %q", first.Stream, runner.StreamStdout)
	}
	if first.TS.IsZero() {
		t.Error("frame has a zero timestamp")
	}
	if !strings.Contains(first.Text, "fake server ready") {
		t.Errorf("first line = %q, want the startup line", first.Text)
	}
}

func TestConsoleHistoryLinesParameter(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"default", "", http.StatusOK},
		{"explicit", "?lines=10", http.StatusOK},
		{"above the cap is clamped, not rejected", "?lines=99999", http.StatusOK},
		{"zero", "?lines=0", http.StatusBadRequest},
		{"negative", "?lines=-5", http.StatusBadRequest},
		{"not a number", "?lines=abc", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do(http.MethodGet,
				"/api/v1/servers/"+testServerID+"/console/history"+tc.query, nil, e.token)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				body := decodeJSON[historyResponse](t, resp)
				if len(body.Items) > MaxHistoryLines {
					t.Fatalf("returned %d lines, exceeds the %d cap", len(body.Items), MaxHistoryLines)
				}
				return
			}
			if code := errorCode(t, resp); code != CodeValidationFailed {
				t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
			}
		})
	}
}

// A server that exists in the database but was never started is unknown to the
// runner, and the console must say so rather than crash.
func TestConsoleHistoryOnAServerThatIsNotRunning(t *testing.T) {
	e := newTestEnv(t)

	record := &store.Server{
		OwnerID: e.user.ID, Name: "not-started", Core: "paper", Version: "1.21.4",
		RAMMb: 1024, Port: 25999, Dir: t.TempDir(),
	}
	if err := e.db.Servers.Create(context.Background(), record); err != nil {
		t.Fatalf("creating server record: %v", err)
	}

	resp := e.do(http.MethodGet,
		"/api/v1/servers/"+record.ID+"/console/history", nil, e.token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- command ---

func TestCommandValidation(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	tests := []struct {
		name       string
		command    string
		wantStatus int
	}{
		{"valid", "list", http.StatusNoContent},
		{"cyrillic", "say Рестарт через 5 минут", http.StatusNoContent},
		{"empty", "", http.StatusBadRequest},
		{"blank", "   ", http.StatusBadRequest},
		{"newline injection", "say hi\nop Steve", http.StatusBadRequest},
		{"carriage return", "say hi\rop Steve", http.StatusBadRequest},
		{"too long", strings.Repeat("a", runner.MaxCommandRunes+1), http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/command",
				commandRequest{Command: tc.command}, e.token)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusBadRequest {
				if code := errorCode(t, resp); code != CodeValidationFailed {
					t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
				}
				return
			}
			_ = resp.Body.Close()
		})
	}
}

func TestCommandRejectsMalformedBody(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	req, _ := http.NewRequest(http.MethodPost,
		e.server.URL+"/api/v1/servers/"+testServerID+"/command",
		strings.NewReader("this is not json"))
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeValidationFailed {
		t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
	}
}

// --- tickets over HTTP ---

func TestConsoleTicketEndpoint(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/console/ticket", nil, e.token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	body := decodeJSON[ticketResponse](t, resp)
	if body.Ticket == "" {
		t.Fatal("response contains no ticket")
	}
	if !body.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at = %v, want a future time", body.ExpiresAt)
	}
	if d := time.Until(body.ExpiresAt); d > TicketTTL+time.Second {
		t.Fatalf("ticket lives for %v, want at most %v", d, TicketTTL)
	}
}

// --- websocket ---

func (e *testEnv) issueTicket(serverID string) string {
	e.t.Helper()

	resp := e.do(http.MethodPost, "/api/v1/servers/"+serverID+"/console/ticket", nil, e.token)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("issuing ticket: status %d", resp.StatusCode)
	}
	return decodeJSON[ticketResponse](e.t, resp).Ticket
}

func (e *testEnv) dialConsole(serverID, ticket string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()

	u, err := url.Parse(e.server.URL)
	if err != nil {
		e.t.Fatalf("parsing test server url: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/api/v1/servers/" + serverID + "/console"
	u.RawQuery = "token=" + url.QueryEscape(ticket)

	return websocket.DefaultDialer.Dial(u.String(), nil)
}

// readFrameTypes reads frames until one satisfies match or the deadline passes.
func readUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("reading frame: %v", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("frame is not json: %v (%s)", err, data)
		}
		if match(frame) {
			return frame
		}
	}
}

func TestConsoleWebSocketRequiresValidTicket(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	t.Run("no ticket", func(t *testing.T) {
		_, resp, err := e.dialConsole(testServerID, "")
		if err == nil {
			t.Fatal("dial succeeded without a ticket")
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %v, want 401", resp)
		}
	})

	t.Run("garbage ticket", func(t *testing.T) {
		_, resp, err := e.dialConsole(testServerID, "not-a-real-ticket")
		if err == nil {
			t.Fatal("dial succeeded with an invalid ticket")
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %v, want 401", resp)
		}
	})

	t.Run("ticket is single use", func(t *testing.T) {
		ticket := e.issueTicket(testServerID)

		conn, _, err := e.dialConsole(testServerID, ticket)
		if err != nil {
			t.Fatalf("first dial: %v", err)
		}
		_ = conn.Close()

		_, resp, err := e.dialConsole(testServerID, ticket)
		if err == nil {
			t.Fatal("the same ticket opened a second connection")
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %v, want 401", resp)
		}
	})
}

// A ticket for one server must not open another server's console.
func TestConsoleWebSocketRejectsTicketForOtherServer(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)
	e.startServer(otherServerID)

	ticket := e.issueTicket(testServerID)

	_, resp, err := e.dialConsole(otherServerID, ticket)
	if err == nil {
		t.Fatal("a ticket for one server opened another server's console")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

func TestConsoleWebSocketSendsHistoryOnConnect(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	frame := readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == frameLine
	})
	if text, _ := frame["text"].(string); !strings.Contains(text, "fake server ready") {
		t.Fatalf("first line frame = %q, want the buffered startup line", text)
	}
}

func TestConsoleWebSocketSendsStatusOnConnect(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	frame := readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == frameStatus
	})
	if got := frame["status"]; got != string(runner.StatusRunning) {
		t.Fatalf("status frame = %v, want %q", got, runner.StatusRunning)
	}
}

// The integration test the task asks for: a command sent over HTTP must show
// up both in the live WebSocket stream and in the history.
func TestConsoleCommandOverHTTPAppearsInWebSocketAndHistory(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Drain the connect burst so the assertion below is about the new line.
	readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == frameStatus
	})

	const command = "say integration"
	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/command",
		commandRequest{Command: command}, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("command status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	frame := readUntil(t, conn, 10*time.Second, func(f map[string]any) bool {
		text, _ := f["text"].(string)
		return f["type"] == frameLine && strings.Contains(text, "echo: "+command)
	})
	if frame["stream"] != runner.StreamStdout {
		t.Errorf("stream = %v, want %q", frame["stream"], runner.StreamStdout)
	}

	historyResp := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/console/history", nil, e.token)
	if historyResp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d, want 200", historyResp.StatusCode)
	}
	body := decodeJSON[historyResponse](t, historyResp)

	found := false
	for _, item := range body.Items {
		if strings.Contains(item.Text, "echo: "+command) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("command echo missing from history (%d lines)", len(body.Items))
	}
}

// The same round trip, but the command is sent over the socket itself.
func TestConsoleCommandOverWebSocket(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == frameStatus
	})

	if err := conn.WriteJSON(clientFrame{Type: frameCommand, Text: "list"}); err != nil {
		t.Fatalf("sending command frame: %v", err)
	}

	readUntil(t, conn, 10*time.Second, func(f map[string]any) bool {
		text, _ := f["text"].(string)
		return f["type"] == frameLine && strings.Contains(text, "echo: list")
	})
}

func TestConsoleWebSocketRejectsInvalidCommandFrames(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	tests := []struct {
		name     string
		send     any
		wantCode string
	}{
		{"newline injection", clientFrame{Type: frameCommand, Text: "say hi\nop Steve"}, CodeValidationFailed},
		{"empty command", clientFrame{Type: frameCommand, Text: ""}, CodeValidationFailed},
		{"too long", clientFrame{Type: frameCommand, Text: strings.Repeat("a", runner.MaxCommandRunes+1)}, CodeValidationFailed},
		{"unknown frame type", clientFrame{Type: "nonsense", Text: "x"}, CodeValidationFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := conn.WriteJSON(tc.send); err != nil {
				t.Fatalf("sending frame: %v", err)
			}
			frame := readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
				return f["type"] == frameError
			})
			if frame["code"] != tc.wantCode {
				t.Fatalf("error code = %v, want %q", frame["code"], tc.wantCode)
			}
		})
	}
}

// Stopping the server must close the socket rather than leave it hanging.
func TestConsoleWebSocketClosesWhenServerStops(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == frameStatus
	})

	if err := e.runner.Stop(context.Background(), testServerID, 5*time.Second); err != nil {
		t.Fatalf("stopping server: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return // the socket closed, as required
		}
	}
}

// Daemon shutdown must release console sockets too.
func TestConsoleWebSocketClosesOnRunnerShutdown(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
		return f["type"] == frameStatus
	})

	if err := e.runner.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutting down runner: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// Several viewers on one server must all receive the same line.
func TestConsoleWebSocketFanOutToMultipleViewers(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	const viewers = 3
	conns := make([]*websocket.Conn, viewers)
	for i := range conns {
		conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = conn.Close() }()
		conns[i] = conn

		readUntil(t, conn, 5*time.Second, func(f map[string]any) bool {
			return f["type"] == frameStatus
		})
	}

	const command = "say everyone"
	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/command",
		commandRequest{Command: command}, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("command status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	for i, conn := range conns {
		readUntil(t, conn, 10*time.Second, func(f map[string]any) bool {
			text, _ := f["text"].(string)
			return f["type"] == frameLine && strings.Contains(text, "echo: "+command)
		})
		_ = i
	}
}

func TestHealthEndpointNeedsNoAuth(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/health", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON[healthResponse](t, resp)
	if body.Status != "ok" {
		t.Fatalf("status field = %q, want ok", body.Status)
	}
}

// decodeJSONRaw returns the response body as text, for assertions about what
// must not appear in it.
func decodeJSONRaw(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return string(raw)
}
