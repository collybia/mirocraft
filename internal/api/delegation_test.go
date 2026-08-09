package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/collybia/mirocraft/internal/store"
)

// linkedBot returns a bot token that may act for others, and links the given
// chat account to a panel user.
func (e *testEnv) linkedBot(t *testing.T, userID, externalID string) string {
	t.Helper()

	botUser := &store.User{Email: "bot@example.com", PasswordHash: "x", Role: store.RoleUser}
	if err := e.db.Users.Create(context.Background(), botUser); err != nil {
		t.Fatalf("creating the bot account: %v", err)
	}
	token := e.mintToken(botUser.ID, []string{ScopeIntegrationsAct})

	if externalID != "" {
		e.link(t, userID, externalID)
	}
	return token
}

// link connects a chat account to a panel account through the real flow: a
// code is issued and redeemed, rather than a row being written behind the
// mechanism the tests are here to check.
func (e *testEnv) link(t *testing.T, userID, externalID string) {
	t.Helper()

	ctx := context.Background()
	code, _, err := e.db.Integrations.IssueCode(ctx, store.ProviderDiscord, userID)
	if err != nil {
		t.Fatalf("issuing a linking code: %v", err)
	}
	if _, err := e.db.Integrations.Redeem(ctx, store.ProviderDiscord, code, externalID); err != nil {
		t.Fatalf("redeeming the linking code: %v", err)
	}
}

// The point of the whole mechanism: a bot's request, made for a linked
// person, is answered with that person's data rather than the bot's.
func TestDelegatedRequestActsAsTheLinkedUser(t *testing.T) {
	e := newTestEnv(t)
	botToken := e.linkedBot(t, e.user.ID, "31337")

	req, err := http.NewRequest(http.MethodGet, e.server.URL+"/api/v1/servers", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set(DelegationHeader, "discord:31337")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("performing the request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body listResponse[serverResponse]
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// The seeded owner has exactly one server; the bot's own account has none.
	if len(body.Items) != 1 || body.Items[0].ID != testServerID {
		t.Fatalf("items = %+v, want the linked user's server", body.Items)
	}
}

// Without the link there is nobody to act as, and the request must not fall
// back to the bot's own account.
func TestDelegationForAnUnlinkedAccountIsRefused(t *testing.T) {
	e := newTestEnv(t)
	botToken := e.linkedBot(t, e.user.ID, "")

	resp := e.doWithDelegation(t, botToken, "discord:99999")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// The escalation this design has to prevent: an ordinary session token must
// not be able to name someone else and become them.
func TestASessionTokenCannotDelegate(t *testing.T) {
	e := newTestEnv(t)
	// e.token is an ordinary API token without the delegation scope, and the
	// admin token holds admin:* — neither may act for anyone.
	e.link(t, e.other.ID, "31337")

	// Minted with the wildcard but without the delegation scope by name,
	// which is the case that matters: admin:* must not stand in for it.
	wildcardOnly := e.mintToken(e.admin.ID, []string{ScopeAdminAll})

	for name, token := range map[string]string{
		"a plain token":               e.token,
		"an admin token with admin:*": wildcardOnly,
	} {
		t.Run(name, func(t *testing.T) {
			resp := e.doWithDelegation(t, token, "discord:31337")
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// Session tokens must not carry the scope in the first place, or the check
// above would be the only thing standing between a login and impersonation.
func TestSessionScopesExcludeDelegation(t *testing.T) {
	for _, role := range []string{RoleAdmin, RoleUser} {
		for _, scope := range scopesForRole(role) {
			if scope == ScopeIntegrationsAct {
				t.Errorf("a %s session token carries %s", role, ScopeIntegrationsAct)
			}
		}
	}
}

// A delegated caller gets what the person may do, capped: an admin acting
// through a bot must not be able to administer anything.
func TestDelegatedScopesAreCapped(t *testing.T) {
	for _, role := range []string{RoleAdmin, RoleUser} {
		scopes := delegatedScopes(role)

		for _, scope := range scopes {
			if scope == ScopeAdminAll {
				t.Errorf("a delegated %s carries %s", role, ScopeAdminAll)
			}
			if scope == ScopeFilesWrite || scope == ScopeBackupsWrite || scope == ScopeServersWrite {
				t.Errorf("a delegated %s carries %s, which is outside the delegatable set", role, scope)
			}
		}
		// And it is not empty, or the mechanism would be useless.
		if len(scopes) == 0 {
			t.Errorf("a delegated %s carries no scopes at all", role)
		}
	}
}

// An admin acting through a bot reaches the admin endpoints with neither the
// role nor the scope, and the header is refused outright rather than ignored.
func TestDelegationIsRefusedOnAdminEndpoints(t *testing.T) {
	e := newTestEnv(t)
	botToken := e.linkedBot(t, e.admin.ID, "31337")

	req, err := http.NewRequest(http.MethodGet, e.server.URL+"/api/v1/admin/users", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set(DelegationHeader, "discord:31337")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("performing the request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMalformedDelegationHeadersAreRefused(t *testing.T) {
	e := newTestEnv(t)
	botToken := e.linkedBot(t, e.user.ID, "31337")

	for _, header := range []string{"31337", "discord:", ":31337", "irc:31337", "   "} {
		t.Run(header, func(t *testing.T) {
			resp := e.doWithDelegation(t, botToken, header)
			defer func() { _ = resp.Body.Close() }()

			// Whitespace is treated as absent, so the request runs as the bot
			// itself — whose token carries only integrations:act and may not
			// list servers. Refused either way, which is the point.
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// A blocked account cannot be acted for, the same as it cannot log in.
func TestDelegationForABlockedUserIsRefused(t *testing.T) {
	e := newTestEnv(t)
	botToken := e.linkedBot(t, e.user.ID, "31337")

	e.user.Blocked = true
	if err := e.db.Users.Update(context.Background(), e.user); err != nil {
		t.Fatalf("blocking the user: %v", err)
	}

	resp := e.doWithDelegation(t, botToken, "discord:31337")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// doWithDelegation performs GET /servers with a token and a delegation header.
func (e *testEnv) doWithDelegation(t *testing.T, token, header string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, e.server.URL+"/api/v1/servers", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(DelegationHeader, header)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("performing the request: %v", err)
	}
	return resp
}
