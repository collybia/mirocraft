package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// --- login ---

func TestLogin(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: e.user.Email, Password: testPassword}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[loginResponse](t, resp)
	if body.Token == "" {
		t.Fatal("login returned no token")
	}
	if !strings.HasPrefix(body.Token, store.TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix", body.Token, store.TokenPrefix)
	}
	if !body.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want a future time", body.ExpiresAt)
	}
	if body.User.Email != e.user.Email {
		t.Errorf("user email = %q, want %q", body.User.Email, e.user.Email)
	}

	// The returned token must actually work.
	me := e.do(http.MethodGet, "/api/v1/auth/me", nil, body.Token)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("using the session token gave %d, want 200", me.StatusCode)
	}
	_ = me.Body.Close()
}

// A wrong password and an unknown address must be indistinguishable, or the
// endpoint becomes an address-enumeration oracle.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newTestEnv(t)

	unknown := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: "nobody@example.com", Password: testPassword}, "")
	wrongPassword := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: e.user.Email, Password: "not the password"}, "")

	if unknown.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown email gave %d, want 401", unknown.StatusCode)
	}
	if wrongPassword.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password gave %d, want 401", wrongPassword.StatusCode)
	}

	unknownBody := decodeJSONRaw(t, unknown)
	wrongBody := decodeJSONRaw(t, wrongPassword)
	if unknownBody != wrongBody {
		t.Fatalf("the two failures differ:\n unknown: %s\n wrong:   %s", unknownBody, wrongBody)
	}
}

func TestLoginRejectsBlockedUser(t *testing.T) {
	e := newTestEnv(t)

	e.user.Blocked = true
	if err := e.db.Users.Update(t.Context(), e.user); err != nil {
		t.Fatalf("blocking user: %v", err)
	}

	resp := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: e.user.Email, Password: testPassword}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A token belonging to a user who is blocked later must stop working.
func TestBlockedUserTokenStopsWorking(t *testing.T) {
	e := newTestEnv(t)

	ok := e.do(http.MethodGet, "/api/v1/auth/me", nil, e.token)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("status before blocking = %d, want 200", ok.StatusCode)
	}
	_ = ok.Body.Close()

	e.user.Blocked = true
	if err := e.db.Users.Update(t.Context(), e.user); err != nil {
		t.Fatalf("blocking user: %v", err)
	}

	after := e.do(http.MethodGet, "/api/v1/auth/me", nil, e.token)
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after blocking = %d, want 401", after.StatusCode)
	}
	_ = after.Body.Close()
}

func TestLoginRateLimited(t *testing.T) {
	e := newTestEnv(t)

	// The documented login allowance is 5 per minute per IP.
	var lastStatus int
	for i := 0; i < DefaultLoginRateLimit+2; i++ {
		resp := e.do(http.MethodPost, "/api/v1/auth/login",
			loginRequest{Email: e.user.Email, Password: "wrong"}, "")
		lastStatus = resp.StatusCode
		if i == DefaultLoginRateLimit {
			if resp.Header.Get("Retry-After") == "" {
				t.Error("a rate-limited response carries no Retry-After header")
			}
			if code := errorCode(t, resp); code != CodeRateLimited {
				t.Errorf("error code = %q, want %q", code, CodeRateLimited)
			}
			continue
		}
		_ = resp.Body.Close()
	}

	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("status after exceeding the limit = %d, want 429", lastStatus)
	}
}

func TestLogoutRevokesTheToken(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/auth/logout", nil, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	after := e.do(http.MethodGet, "/api/v1/auth/me", nil, e.token)
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the revoked token still works: status %d", after.StatusCode)
	}
	_ = after.Body.Close()
}

func TestRefreshIssuesNewTokenAndRevokesTheOld(t *testing.T) {
	e := newTestEnv(t)

	login := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: e.user.Email, Password: testPassword}, "")
	session := decodeJSON[loginResponse](t, login).Token

	resp := e.do(http.MethodPost, "/api/v1/auth/refresh", nil, session)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	refreshed := decodeJSON[loginResponse](t, resp).Token

	if refreshed == session {
		t.Fatal("refresh returned the same token")
	}

	old := e.do(http.MethodGet, "/api/v1/auth/me", nil, session)
	if old.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old token still works after refresh: status %d", old.StatusCode)
	}
	_ = old.Body.Close()

	fresh := e.do(http.MethodGet, "/api/v1/auth/me", nil, refreshed)
	if fresh.StatusCode != http.StatusOK {
		t.Fatalf("the refreshed token does not work: status %d", fresh.StatusCode)
	}
	_ = fresh.Body.Close()
}

// An expired token must be rejected even though the row still exists.
func TestExpiredTokenRejected(t *testing.T) {
	e := newTestEnv(t)

	value, hash, err := store.GenerateToken()
	if err != nil {
		t.Fatalf("generating token: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	err = e.db.Tokens.Create(t.Context(), &store.Token{
		UserID: e.user.ID, Hash: hash, Scopes: []string{ScopeServersRead}, ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	resp := e.do(http.MethodGet, "/api/v1/auth/me", nil, value)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- me ---

func TestMeReturnsProfileAndScopes(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/users/me", nil, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[meResponse](t, resp)
	if body.Email != e.user.Email {
		t.Errorf("email = %q, want %q", body.Email, e.user.Email)
	}
	if body.Theme != ThemeSystem {
		t.Errorf("theme = %q, want the %q default", body.Theme, ThemeSystem)
	}
	if len(body.Scopes) != 2 {
		t.Errorf("scopes = %v, want the token's two", body.Scopes)
	}
}

func TestPatchMeTheme(t *testing.T) {
	e := newTestEnv(t)

	theme := "midnight"
	resp := e.do(http.MethodPatch, "/api/v1/users/me", patchMeRequest{Theme: &theme}, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := decodeJSON[userResponse](t, resp).Theme; got != theme {
		t.Fatalf("theme = %q, want %q", got, theme)
	}

	// The choice must survive, since it lives in the profile rather than the
	// browser.
	stored, err := e.db.Users.GetByID(t.Context(), e.user.ID)
	if err != nil {
		t.Fatalf("reading user: %v", err)
	}
	if stored.Theme != theme {
		t.Fatalf("stored theme = %q, want %q", stored.Theme, theme)
	}
}

func TestPatchMeRejectsUnknownTheme(t *testing.T) {
	e := newTestEnv(t)

	theme := "neon-hotdog"
	resp := e.do(http.MethodPatch, "/api/v1/users/me", patchMeRequest{Theme: &theme}, e.token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeValidationFailed {
		t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
	}
}

// Changing a password must require proving the current one, so a stolen
// session cannot lock the owner out.
func TestPatchMePasswordRequiresTheOldOne(t *testing.T) {
	e := newTestEnv(t)

	newPassword := "a brand new passphrase"

	without := e.do(http.MethodPatch, "/api/v1/users/me",
		patchMeRequest{Password: &newPassword}, e.token)
	if without.StatusCode != http.StatusBadRequest {
		t.Fatalf("changing the password without the old one gave %d, want 400", without.StatusCode)
	}
	_ = without.Body.Close()

	wrong := "not the current password"
	withWrong := e.do(http.MethodPatch, "/api/v1/users/me",
		patchMeRequest{Password: &newPassword, OldPassword: &wrong}, e.token)
	if withWrong.StatusCode != http.StatusBadRequest {
		t.Fatalf("changing the password with a wrong old one gave %d, want 400", withWrong.StatusCode)
	}
	_ = withWrong.Body.Close()

	old := testPassword
	ok := e.do(http.MethodPatch, "/api/v1/users/me",
		patchMeRequest{Password: &newPassword, OldPassword: &old}, e.token)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("changing the password correctly gave %d, want 200", ok.StatusCode)
	}
	_ = ok.Body.Close()

	login := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: e.user.Email, Password: newPassword}, "")
	if login.StatusCode != http.StatusOK {
		t.Fatalf("logging in with the new password gave %d, want 200", login.StatusCode)
	}
	_ = login.Body.Close()
}

func TestPatchMeRejectsShortPassword(t *testing.T) {
	e := newTestEnv(t)

	short := "short"
	old := testPassword
	resp := e.do(http.MethodPatch, "/api/v1/users/me",
		patchMeRequest{Password: &short, OldPassword: &old}, e.token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPatchMeRejectsInvalidEmail(t *testing.T) {
	e := newTestEnv(t)

	bad := "not-an-email"
	resp := e.do(http.MethodPatch, "/api/v1/users/me", patchMeRequest{Email: &bad}, e.token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPatchMeRejectsTakenEmail(t *testing.T) {
	e := newTestEnv(t)

	taken := e.other.Email
	resp := e.do(http.MethodPatch, "/api/v1/users/me", patchMeRequest{Email: &taken}, e.token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeValidationFailed {
		t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
	}
}

// --- api tokens ---

func TestCreateAndUseAPIToken(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/auth/tokens",
		createTokenRequest{Name: "ci", Scopes: []string{ScopeServersRead}}, e.token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	body := decodeJSON[createTokenResponse](t, resp)
	if body.Token == "" {
		t.Fatal("the response carries no token value")
	}
	if body.ID == "" {
		t.Fatal("the response carries no token id")
	}

	me := e.do(http.MethodGet, "/api/v1/auth/me", nil, body.Token)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("the new token does not work: status %d", me.StatusCode)
	}
	_ = me.Body.Close()
}

// The value is shown once; listing tokens must never return it again.
func TestListTokensNeverReturnsValues(t *testing.T) {
	e := newTestEnv(t)

	created := e.do(http.MethodPost, "/api/v1/auth/tokens",
		createTokenRequest{Name: "ci", Scopes: []string{ScopeServersRead}}, e.token)
	value := decodeJSON[createTokenResponse](t, created).Token

	resp := e.do(http.MethodGet, "/api/v1/auth/tokens", nil, e.token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	raw := decodeJSONRaw(t, resp)
	if strings.Contains(raw, value) {
		t.Fatal("the token list contains a token value")
	}
}

// A token must not be able to mint one with scopes its creator lacks.
func TestCreateTokenCannotEscalateScopes(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/auth/tokens",
		createTokenRequest{Name: "escalate", Scopes: []string{ScopeAdminAll}}, e.token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeValidationFailed {
		t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
	}
}

func TestCreateTokenValidation(t *testing.T) {
	e := newTestEnv(t)

	past := time.Now().Add(-time.Hour)
	tests := []struct {
		name string
		req  createTokenRequest
	}{
		{"no name", createTokenRequest{Scopes: []string{ScopeServersRead}}},
		{"no scopes", createTokenRequest{Name: "x"}},
		{"unknown scope", createTokenRequest{Name: "x", Scopes: []string{"servers:teleport"}}},
		{"expiry in the past", createTokenRequest{Name: "x", Scopes: []string{ScopeServersRead}, ExpiresAt: &past}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do(http.MethodPost, "/api/v1/auth/tokens", tc.req, e.token)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestDeleteToken(t *testing.T) {
	e := newTestEnv(t)

	created := e.do(http.MethodPost, "/api/v1/auth/tokens",
		createTokenRequest{Name: "ci", Scopes: []string{ScopeServersRead}}, e.token)
	body := decodeJSON[createTokenResponse](t, created)

	resp := e.do(http.MethodDelete, "/api/v1/auth/tokens/"+body.ID, nil, e.token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	after := e.do(http.MethodGet, "/api/v1/auth/me", nil, body.Token)
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the revoked token still works: status %d", after.StatusCode)
	}
	_ = after.Body.Close()
}

// Another user's token id must look like it does not exist.
func TestDeleteTokenOfAnotherUser(t *testing.T) {
	e := newTestEnv(t)

	_, hash, err := store.GenerateToken()
	if err != nil {
		t.Fatalf("generating token: %v", err)
	}
	foreign := &store.Token{UserID: e.other.ID, Name: "theirs", Hash: hash,
		Scopes: []string{ScopeServersRead}}
	if err := e.db.Tokens.Create(t.Context(), foreign); err != nil {
		t.Fatalf("creating token: %v", err)
	}

	resp := e.do(http.MethodDelete, "/api/v1/auth/tokens/"+foreign.ID, nil, e.token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- admin ---

func TestAdminEndpointsRejectNonAdmins(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/admin/users", nil, e.token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeForbidden {
		t.Fatalf("error code = %q, want %q", code, CodeForbidden)
	}
}

func TestAdminListUsers(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/admin/users", nil, e.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[listResponse[userResponse]](t, resp)
	if len(body.Items) != 3 {
		t.Fatalf("listed %d users, want the 3 fixtures", len(body.Items))
	}
	for _, u := range body.Items {
		if u.ID == "" || u.Email == "" {
			t.Errorf("user entry is incomplete: %+v", u)
		}
	}
}

func TestAdminCreateUser(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/admin/users", createUserRequest{
		Email: "new@example.com", Password: "a sufficiently long password",
		Role: RoleUser, MaxServers: 3,
	}, e.adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	body := decodeJSON[userResponse](t, resp)
	if body.Email != "new@example.com" || body.MaxServers != 3 {
		t.Fatalf("created user = %+v", body)
	}

	login := e.do(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: "new@example.com", Password: "a sufficiently long password"}, "")
	if login.StatusCode != http.StatusOK {
		t.Fatalf("the new user cannot log in: status %d", login.StatusCode)
	}
	_ = login.Body.Close()
}

func TestAdminCreateUserValidation(t *testing.T) {
	e := newTestEnv(t)

	tests := []struct {
		name string
		req  createUserRequest
	}{
		{"bad email", createUserRequest{Email: "nope", Password: "a long enough password"}},
		{"short password", createUserRequest{Email: "x@example.com", Password: "short"}},
		{"unknown role", createUserRequest{Email: "x@example.com", Password: "a long enough password", Role: "root"}},
		{"duplicate email", createUserRequest{Email: "owner@example.com", Password: "a long enough password"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do(http.MethodPost, "/api/v1/admin/users", tc.req, e.adminToken)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestAdminPatchUser(t *testing.T) {
	e := newTestEnv(t)

	blocked := true
	limit := 7
	resp := e.do(http.MethodPatch, "/api/v1/admin/users/"+e.user.ID,
		patchUserRequest{Blocked: &blocked, MaxServers: &limit}, e.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[userResponse](t, resp)
	if !body.Blocked || body.MaxServers != 7 {
		t.Fatalf("patched user = %+v", body)
	}
}

// An admin must not be able to lock the panel out of its administrators by
// demoting or blocking themselves.
func TestAdminCannotDemoteOrBlockThemselves(t *testing.T) {
	e := newTestEnv(t)

	role := RoleUser
	demote := e.do(http.MethodPatch, "/api/v1/admin/users/"+e.admin.ID,
		patchUserRequest{Role: &role}, e.adminToken)
	if demote.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-demotion gave %d, want 400", demote.StatusCode)
	}
	_ = demote.Body.Close()

	blocked := true
	block := e.do(http.MethodPatch, "/api/v1/admin/users/"+e.admin.ID,
		patchUserRequest{Blocked: &blocked}, e.adminToken)
	if block.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-blocking gave %d, want 400", block.StatusCode)
	}
	_ = block.Body.Close()

	remove := e.do(http.MethodDelete, "/api/v1/admin/users/"+e.admin.ID, nil, e.adminToken)
	if remove.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-deletion gave %d, want 400", remove.StatusCode)
	}
	_ = remove.Body.Close()
}

func TestAdminDeleteUser(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodDelete, "/api/v1/admin/users/"+e.other.ID, nil, e.adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := e.db.Users.GetByID(t.Context(), e.other.ID); err == nil {
		t.Fatal("the user still exists after deletion")
	}
}

// --- audit ---

// Mutating actions must leave an audit trail.
func TestMutatingActionsAreAudited(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/auth/tokens",
		createTokenRequest{Name: "audited", Scopes: []string{ScopeServersRead}}, e.token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	_ = resp.Body.Close()

	entries, err := e.db.Audit.List(t.Context(), e.user.ID, 10)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}

	found := false
	for _, entry := range entries {
		if entry.Action == "token.create" {
			found = true
			if entry.IP == "" {
				t.Error("the audit entry records no client address")
			}
		}
	}
	if !found {
		t.Fatalf("token.create is missing from the audit log (%d entries)", len(entries))
	}
}
