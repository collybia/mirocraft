package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/mcping"
	"github.com/collybia/mirocraft/internal/store"
)

// Regression tests for defects found by review. Each one fails against the
// code as it was, so the defect cannot come back unnoticed.

// A console disconnect that races a burst of log lines used to close the
// outbound channel while the fan-out goroutine could still send on it, which
// panics and takes the whole daemon down. The test opens and drops sockets
// while the server is chatty, which is exactly that race.
func TestConsoleDisconnectDoesNotPanicUnderLoad(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	// Keep the server producing output for the whole test.
	stopChatter := make(chan struct{})
	var chatter sync.WaitGroup
	chatter.Add(1)
	go func() {
		defer chatter.Done()
		for i := 0; ; i++ {
			select {
			case <-stopChatter:
				return
			default:
			}
			_ = e.runner.SendCommand(context.Background(), testServerID, "spam line")
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stopChatter)
		chatter.Wait()
	})

	// Open and immediately drop a socket, repeatedly. A panic in the fan-out
	// goroutine would kill the test binary outright.
	for i := 0; i < 25; i++ {
		conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		// Read a little so the fan-out is genuinely active, then drop it
		// mid-flight rather than closing politely.
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, _ = conn.ReadMessage()
		_ = conn.Close()
	}

	// The daemon must still be serving.
	resp := e.do(http.MethodGet, "/api/v1/health", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health after the disconnect storm = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// Closing a socket while frames are queued must not panic either.
func TestConsoleCloseWithQueuedFramesIsClean(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	conn, _, err := e.dialConsole(testServerID, e.issueTicket(testServerID))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Never read, so frames pile up in the outbound queue, then close.
	for i := 0; i < 50; i++ {
		_ = e.runner.SendCommand(context.Background(), testServerID, "queued line")
	}
	time.Sleep(100 * time.Millisecond)
	_ = conn.Close()
	time.Sleep(200 * time.Millisecond)

	resp := e.do(http.MethodGet, "/api/v1/health", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health after closing a backed-up socket = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// The per-user memory quota was enforced on create but not on patch, so a
// user could create a small server and then raise it past their allowance.
func TestPatchServerCannotEscapeTheRAMQuota(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	// The fixture server already allocates 1024 MB.
	e.user.MaxRAMMb = 2048
	if err := e.db.Users.Update(t.Context(), e.user); err != nil {
		t.Fatalf("updating user: %v", err)
	}

	tooMuch := 524288
	resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		patchServerRequest{RAMMb: &tooMuch}, token)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "insufficient_resources" {
		t.Fatalf("error code = %q, want insufficient_resources", code)
	}

	stored, err := e.db.Servers.GetByID(t.Context(), testServerID)
	if err != nil {
		t.Fatalf("reading server: %v", err)
	}
	if stored.RAMMb != 1024 {
		t.Fatalf("ram_mb = %d after a rejected patch, want it unchanged", stored.RAMMb)
	}
}

// Staying within the allowance must still work, and lowering RAM must never
// be blocked by the quota.
func TestPatchServerRAMWithinQuotaIsAllowed(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	e.user.MaxRAMMb = 4096
	if err := e.db.Users.Update(t.Context(), e.user); err != nil {
		t.Fatalf("updating user: %v", err)
	}

	up := 4096
	resp := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		patchServerRequest{RAMMb: &up}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raising to exactly the allowance gave %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	down := 512
	lower := e.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		patchServerRequest{RAMMb: &down}, token)
	if lower.StatusCode != http.StatusOK {
		t.Fatalf("lowering ram gave %d, want 200", lower.StatusCode)
	}
	_ = lower.Body.Close()
}

// An email was stored exactly as sent while only a trimmed, parsed copy was
// validated, so a padded or display-name address locked the account out of
// its own login.
func TestEmailIsNormalizedBeforeStoring(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"trailing space", "renamed@example.com ", "renamed@example.com"},
		{"leading space", "  spaced@example.com", "spaced@example.com"},
		{"display name form", "Alice <alice@example.com>", "alice@example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)

			resp := e.do(http.MethodPatch, "/api/v1/users/me",
				patchMeRequest{Email: &tc.input}, e.token)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := decodeJSON[userResponse](t, resp).Email; got != tc.want {
				t.Fatalf("stored email = %q, want %q", got, tc.want)
			}

			// The whole point: the account must still be able to log in.
			login := e.do(http.MethodPost, "/api/v1/auth/login",
				loginRequest{Email: tc.want, Password: testPassword}, "")
			if login.StatusCode != http.StatusOK {
				t.Fatalf("login after the email change gave %d, want 200", login.StatusCode)
			}
			_ = login.Body.Close()
		})
	}
}

func TestAdminCreatedEmailIsNormalized(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/admin/users", createUserRequest{
		Email: "Bob <bob@example.com>", Password: "a sufficiently long password",
	}, e.adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := decodeJSON[userResponse](t, resp).Email; got != "bob@example.com" {
		t.Fatalf("stored email = %q, want bob@example.com", got)
	}

	login := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: "bob@example.com", Password: "a sufficiently long password"}, "")
	if login.StatusCode != http.StatusOK {
		t.Fatalf("the new user cannot log in: status %d", login.StatusCode)
	}
	_ = login.Body.Close()
}

// PATCH decoded into a struct pre-filled from the stored theme, and
// json.Unmarshal merges into a non-nil map, so a removed variable survived
// every save.
func TestPatchThemeCanRemoveAVariable(t *testing.T) {
	e := newTestEnv(t)

	created := e.do(http.MethodPost, "/api/v1/users/me/themes", ThemeDocument{
		Schema: ThemeSchema, Name: "Two vars", Base: ThemeBaseDark,
		Vars: map[string]string{"--accent": "#7c5cff", "--bg": "#101010"},
	}, e.token)
	theme := decodeJSON[customThemeResponse](t, created)

	// Save with --accent removed.
	patched := e.do(http.MethodPatch, "/api/v1/users/me/themes/"+theme.ID, ThemeDocument{
		Schema: ThemeSchema, Name: "Two vars", Base: ThemeBaseDark,
		Vars: map[string]string{"--bg": "#101010"},
	}, e.token)
	if patched.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", patched.StatusCode)
	}

	got := decodeJSON[customThemeResponse](t, patched)
	if _, still := got.Vars["--accent"]; still {
		t.Fatalf("--accent survived its removal: %v", got.Vars)
	}
	if got.Vars["--bg"] != "#101010" {
		t.Fatalf("--bg was lost: %v", got.Vars)
	}

	// And it must be gone from storage, not just from the response.
	stored, err := e.db.CustomThemes.GetByID(t.Context(), theme.ID)
	if err != nil {
		t.Fatalf("reading theme: %v", err)
	}
	if _, still := stored.Vars["--accent"]; still {
		t.Fatalf("--accent is still stored: %v", stored.Vars)
	}
}

// The name pattern required a leading and a trailing character class, which
// silently imposed a two-character minimum while the error message claimed
// the problem was the character set.
func TestSingleCharacterServerNameIsAccepted(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	req := validCreate()
	req.Name = "A"

	resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := decodeJSON[serverResponse](t, resp).Name; got != "A" {
		t.Fatalf("name = %q, want A", got)
	}
}

// Names that are genuinely unsafe must still be refused.
func TestUnsafeServerNamesStillRejected(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	for _, name := range []string{"..", "-", "_", "a/b", "../etc", "a\b", "."} {
		req := validCreate()
		req.Name = name

		resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q gave %d, want 400", name, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// A padded name is accepted but stored trimmed, so the confirmation required
// to delete the server matches what the user typed.
func TestServerNameIsStoredTrimmed(t *testing.T) {
	e := newTestEnv(t)
	token := e.writeToken()

	req := validCreate()
	req.Name = "  padded  "

	resp := e.do(http.MethodPost, "/api/v1/servers", req, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	created := decodeJSON[serverResponse](t, resp)
	if created.Name != "padded" {
		t.Fatalf("stored name = %q, want it trimmed", created.Name)
	}

	// The confirmation must accept the trimmed name the user sees.
	deleted := e.do(http.MethodDelete,
		"/api/v1/servers/"+created.ID+"?confirm=padded", nil, token)
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete with the displayed name gave %d, want 204", deleted.StatusCode)
	}
	_ = deleted.Body.Close()
}

// Power and delete called the runner without the nil guard every other reader
// applies, so an API built without a Lifecycle panicked instead of answering.
func TestPowerWithoutARunnerFailsCleanly(t *testing.T) {
	e := newTestEnv(t)

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nolifecycle.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hash, err := store.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	user := &store.User{Email: "solo@example.com", PasswordHash: hash, Role: store.RoleUser}
	if err := db.Users.Create(t.Context(), user); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	value, tokenHash, _ := store.GenerateToken()
	if err := db.Tokens.Create(t.Context(), &store.Token{
		UserID: user.ID, Hash: tokenHash, Scopes: AllScopes,
	}); err != nil {
		t.Fatalf("creating token: %v", err)
	}
	srv := &store.Server{
		OwnerID: user.ID, Name: "solo", Core: "paper", Version: "1.21.4",
		RAMMb: 1024, Port: 25599, Dir: t.TempDir(),
	}
	if err := db.Servers.Create(t.Context(), srv); err != nil {
		t.Fatalf("creating server: %v", err)
	}

	// Deliberately no Lifecycle.
	solo := New(Options{Store: db, Logger: e.api.log})
	server := httptest.NewServer(solo.Handler())
	t.Cleanup(server.Close)

	body, err := json.Marshal(powerRequest{Action: ActionStart})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/v1/servers/"+srv.ID+"/power", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+value)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 503 (body: %s)", resp.StatusCode, raw)
	}
}

// Answering a login for an unknown address without hashing returned in
// microseconds while a known address cost a bcrypt round, which is the same
// enumeration oracle the identical response bodies exist to prevent.
func TestLoginTakesSimilarTimeForKnownAndUnknownAddresses(t *testing.T) {
	e := newTestEnv(t)

	measure := func(email string) time.Duration {
		start := time.Now()
		resp := e.do(http.MethodPost, "/api/v1/auth/login",
			loginRequest{Email: email, Password: "definitely the wrong password"}, "")
		_ = resp.Body.Close()
		return time.Since(start)
	}

	// Warm the path so the first bcrypt run does not skew the comparison.
	measure(e.user.Email)

	known := measure(e.user.Email)
	unknown := measure("nobody-here@example.com")

	// bcrypt dominates both, so the two should be within the same order of
	// magnitude. A missing hash on the unknown path shows up as a difference
	// of 100x or more, which this catches without being flaky about jitter.
	ratio := float64(known) / float64(unknown)
	if ratio > 10 || ratio < 0.1 {
		t.Fatalf("login timing differs by %.1fx (known %v, unknown %v) — "+
			"an unknown address is answered without hashing, which enumerates accounts",
			ratio, known, unknown)
	}
}

// The rate limiter keyed its map on the raw bearer token, leaving secrets in
// long-lived memory for no reason.
func TestRateLimiterDoesNotKeyOnRawTokens(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	const raw = "mcr_super_secret_token_value"
	req.Header.Set("Authorization", "Bearer "+raw)

	key := tokenKey(req)
	if strings.Contains(key, raw) {
		t.Fatalf("the limiter key contains the raw token: %q", key)
	}
	if !strings.HasPrefix(key, "token:") {
		t.Fatalf("key = %q, want it to identify the token", key)
	}

	// Different tokens must still land in different buckets.
	other, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	other.Header.Set("Authorization", "Bearer mcr_a_different_token")
	if tokenKey(other) == key {
		t.Fatal("two different tokens share a rate-limit bucket")
	}
}

// The panel is installed for local use and sends no mail, so a plain login
// must work alongside an address.
func TestPlainLoginIsAccepted(t *testing.T) {
	e := newTestEnv(t)

	const login = "petya_2"
	resp := e.do(http.MethodPost, "/api/v1/admin/users", createUserRequest{
		Email: login, Password: "a sufficiently long password",
	}, e.adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := decodeJSON[userResponse](t, resp).Email; got != login {
		t.Fatalf("stored login = %q, want %q", got, login)
	}

	authed := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: login, Password: "a sufficiently long password"}, "")
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("logging in with a plain login gave %d, want 200", authed.StatusCode)
	}
	_ = authed.Body.Close()
}

func TestLoginIdentifierValidation(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		invalid bool
	}{
		{"plain login", "admin", "admin", false},
		{"login with separators", "pe.tya_2-x", "pe.tya_2-x", false},
		{"single character", "a", "a", false},
		{"address", "me@example.com", "me@example.com", false},
		{"address with display name", "Me <me@example.com>", "me@example.com", false},
		{"padded address", "  me@example.com ", "me@example.com", false},
		{"padded login", "  admin  ", "admin", false},

		{"empty", "", "", true},
		{"blank", "   ", "", true},
		{"leading separator", "_admin", "", true},
		{"trailing separator", "admin-", "", true},
		{"space inside", "ad min", "", true},
		{"slash", "ad/min", "", true},
		{"broken address", "not@an@address", "", true},
		{"at with nothing else", "@", "", true},
		{"too long", strings.Repeat("a", 255), "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeLogin(tc.input)
			if tc.invalid {
				if err == nil {
					t.Fatalf("normalizeLogin(%q) = %q, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeLogin(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeLogin(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// A client that sends a command in the wrong encoding must be told so.
//
// Without this, encoding/json replaces the invalid bytes with U+FFFD, the
// command passes validation, and the server prints question marks — with
// nothing anywhere explaining why. Found while chasing exactly that symptom,
// which turned out to be a cp1251 test shell rather than a panel bug.
func TestCommandRejectsNonUTF8Body(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	// "привет" encoded as cp1251, which is not valid UTF-8.
	cp1251 := []byte{0xEF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2}
	body := append([]byte(`{"command":"say `), cp1251...)
	body = append(body, []byte(`"}`)...)

	req, err := http.NewRequest(http.MethodPost,
		e.server.URL+"/api/v1/servers/"+testServerID+"/command", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
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

// Correctly encoded Cyrillic must go through untouched.
func TestCommandAcceptsCyrillic(t *testing.T) {
	e := newTestEnv(t)
	e.startServer(testServerID)

	const command = "say привет из панели"

	ch, unsubscribe, err := e.runner.Subscribe(t.Context(), testServerID)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer unsubscribe()

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/command",
		commandRequest{Command: command}, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatal("the console closed before the echo arrived")
			}
			if strings.Contains(line.Text, "привет из панели") {
				return // arrived intact
			}
			if strings.Contains(line.Text, "�") {
				t.Fatalf("the command was mangled on the way through: %q", line.Text)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the echoed command")
		}
	}
}

// The server directory used to be stored as an absolute path fixed at
// creation time, so moving the data directory — relocating it, restoring a
// backup on another host, changing --data-dir — left every server pointing at
// somewhere that no longer existed. Found by copying a live data directory
// and watching the file listing return four of twenty-three entries.
func TestServersSurviveMovingTheDataDirectory(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	// Confirm the file API works where the data currently lives.
	before := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/files?path=/", nil, token)
	if before.StatusCode != http.StatusOK {
		t.Fatalf("listing before the move gave %d", before.StatusCode)
	}
	countBefore := len(decodeJSON[listFilesResponse](t, before).Items)
	if countBefore == 0 {
		t.Fatal("the fixture has no files to speak of")
	}

	// Move the whole data directory, exactly as an operator would.
	moved := t.TempDir()
	if err := copyDirForTest(e.api.dataDir, moved); err != nil {
		t.Fatalf("copying the data directory: %v", err)
	}

	relocated := New(Options{
		Store:   e.db,
		Console: e.runner,
		DataDir: moved,
		Logger:  e.api.log,
		Ping: func(context.Context, string, int) (*mcping.Status, error) {
			return nil, errors.New("no server is listening in tests")
		},
	})
	server := httptest.NewServer(relocated.Handler())
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/servers/"+testServerID+"/files?path=/", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing after the move gave %d, want 200 — the server was orphaned",
			resp.StatusCode)
	}
	if got := len(decodeJSON[listFilesResponse](t, resp).Items); got != countBefore {
		t.Fatalf("listing after the move has %d entries, before it had %d", got, countBefore)
	}
}

// copyDirForTest duplicates a directory tree.
func copyDirForTest(from, to string) error {
	return filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)

		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			// The open database file may be locked; the server directories
			// are what this test is about.
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o640)
	})
}
