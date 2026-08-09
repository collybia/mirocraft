package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/store"
)

// login returns a session token, which is what a browser holds — as opposed
// to the minted API token the rest of the suite uses.
func (e *testEnv) login(t *testing.T, email, password string) string {
	t.Helper()

	resp := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: email, Password: password}, "")
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("logging in gave %d", resp.StatusCode)
	}
	return decodeJSON[loginResponse](t, resp).Token
}

// A password is changed by someone who thinks a session was stolen. One that
// kept working until its own expiry would make the change theatre.
func TestChangingAPasswordRevokesTheOtherSessions(t *testing.T) {
	env := newTestEnv(t)

	// Two sessions for the same account, as two browsers would be.
	first := env.login(t, env.user.Email, testPassword)
	second := env.login(t, env.user.Email, testPassword)

	resp := env.do(http.MethodPatch, "/api/v1/users/me", map[string]any{
		"old_password": testPassword,
		"password":     "a-new-and-long-enough-password",
	}, first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("changing the password gave %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The session that made the change still works: logging someone out of
	// the tab they just used reads as the change having failed.
	kept := env.do(http.MethodGet, "/api/v1/users/me", nil, first)
	if kept.StatusCode != http.StatusOK {
		t.Errorf("the session that changed the password was revoked: %d", kept.StatusCode)
	}
	_ = kept.Body.Close()

	gone := env.do(http.MethodGet, "/api/v1/users/me", nil, second)
	if gone.StatusCode != http.StatusUnauthorized {
		t.Errorf("the other session still works: %d, want 401", gone.StatusCode)
	}
	_ = gone.Body.Close()
}

// An administrator resetting a password is usually doing it because the
// account is in the wrong hands. Every session of that account goes.
func TestAnAdminResetRevokesEverySession(t *testing.T) {
	env := newTestEnv(t)
	session := env.login(t, env.user.Email, testPassword)

	resp := env.do(http.MethodPatch, "/api/v1/admin/users/"+env.user.ID,
		map[string]any{"password": "another-long-enough-password"}, env.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the reset gave %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	gone := env.do(http.MethodGet, "/api/v1/users/me", nil, session)
	if gone.StatusCode != http.StatusUnauthorized {
		t.Errorf("the reset account's session still works: %d, want 401", gone.StatusCode)
	}
	_ = gone.Body.Close()
}

// An API token is a credential its owner minted on purpose, listed in the
// panel and revocable one by one. A password change is not a reason to break
// every script the account runs.
func TestChangingAPasswordKeepsApiTokens(t *testing.T) {
	env := newTestEnv(t)
	apiToken := env.mintToken(env.user.ID, []string{ScopeServersRead})
	session := env.login(t, env.user.Email, testPassword)

	resp := env.do(http.MethodPatch, "/api/v1/users/me", map[string]any{
		"old_password": testPassword,
		"password":     "yet-another-long-password",
	}, session)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("changing the password gave %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	still := env.do(http.MethodGet, "/api/v1/servers", nil, apiToken)
	if still.StatusCode != http.StatusOK {
		t.Errorf("an api token was revoked by a password change: %d", still.StatusCode)
	}
	_ = still.Body.Close()
}

// The panel has one-click buttons that stop a server and delete it, which is
// exactly what clickjacking is for.
func TestResponsesCarryTheSecurityHeaders(t *testing.T) {
	env := newTestEnv(t)

	resp := env.do(http.MethodGet, "/api/v1/servers", nil, env.token)
	defer func() { _ = resp.Body.Close() }()

	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}

// The panel offers a per-account disk allowance. One that is stored and never
// applied is a limit an operator believes in and does not have.
func TestTheDiskAllowanceIsEnforced(t *testing.T) {
	env := newTestEnv(t)

	// One megabyte, and a server directory already holding most of it.
	env.user.MaxDiskMb = 1
	if err := env.db.Users.Update(context.Background(), env.user); err != nil {
		t.Fatalf("setting the allowance: %v", err)
	}
	dir := env.api.serverDir(env.serverRecord)
	if err := os.WriteFile(filepath.Join(dir, "world.dat"), make([]byte, 900<<10), 0o600); err != nil {
		t.Fatalf("filling the directory: %v", err)
	}

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeFilesWrite})
	resp := env.do(http.MethodPut,
		"/api/v1/servers/"+testServerID+"/files/content?path=/big.txt",
		map[string]any{"content": strings.Repeat("x", 200<<10)}, token)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "insufficient_resources" {
		t.Errorf("code = %q", code)
	}
}

// Zero means unlimited, which is what a single-user panel runs as, and that
// case must not start walking directories to prove it.
func TestNoDiskAllowanceMeansNoLimit(t *testing.T) {
	env := newTestEnv(t)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeFilesWrite})
	resp := env.do(http.MethodPut,
		"/api/v1/servers/"+testServerID+"/files/content?path=/notes.txt",
		map[string]any{"content": "hello"}, token)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, measured := env.api.diskUsage.get(env.user.ID); measured {
		t.Error("an account with no allowance had its disk measured")
	}
}

// A backup is a copy of the world and counts against the same allowance:
// otherwise an account that cannot fill the disk with worlds fills it with
// copies of them.
func TestBackupsCountTowardsTheDiskAllowance(t *testing.T) {
	env := newTestEnv(t)

	env.user.MaxDiskMb = 1
	if err := env.db.Users.Update(context.Background(), env.user); err != nil {
		t.Fatalf("setting the allowance: %v", err)
	}
	// No files on disk at all; the whole allowance is used by a past backup.
	record := &store.Backup{
		ServerID: testServerID, State: store.BackupDone, SizeBytes: 2 << 20,
	}
	if err := env.db.Backups.Create(context.Background(), record); err != nil {
		t.Fatalf("recording a backup: %v", err)
	}
	if err := env.db.Backups.Finish(context.Background(), record.ID,
		store.BackupDone, "backup.tar.gz", 2<<20); err != nil {
		t.Fatalf("finishing the backup: %v", err)
	}

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeBackupsWrite})
	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/backups", nil, token)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}
