package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/collybia/mirocraft/internal/store"
)

// fakeSupervisor records what the API asked it to do, so the tests can check
// that saving a token actually reaches something.
type fakeSupervisor struct {
	mu        sync.Mutex
	syncs     int
	restarts  []string
	connected map[string]bool
}

func (f *fakeSupervisor) Sync(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
}

func (f *fakeSupervisor) Restart(_ context.Context, provider string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, provider)
}

func (f *fakeSupervisor) Running(provider string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected[provider]
}

func (f *fakeSupervisor) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs, append([]string{}, f.restarts...)
}

// withSupervisor attaches a fake supervisor to the environment's API.
func (e *testEnv) withSupervisor() *fakeSupervisor {
	fake := &fakeSupervisor{connected: map[string]bool{}}
	e.api.bots = fake
	return fake
}

// The page has to offer a row for every platform, configured or not, or an
// operator has nowhere to paste their first token.
func TestListingBotsCoversEveryPlatform(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/admin/bots", nil, e.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[listResponse[botResponse]](t, resp)
	if len(body.Items) != 2 {
		t.Fatalf("items = %+v, want one per platform", body.Items)
	}
	for _, item := range body.Items {
		if item.Configured {
			t.Errorf("%s reports a token before one was saved", item.Provider)
		}
	}
}

// The whole reason the settings live in the panel: paste a token, flip the
// switch, and something starts.
func TestSavingATokenStartsTheBot(t *testing.T) {
	e := newTestEnv(t)
	fake := e.withSupervisor()

	resp := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"token": "MTIzNDU2Nzg5.abcdef.ghijkl", "enabled": true}, e.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[botResponse](t, resp)
	if !body.Configured || !body.Enabled {
		t.Fatalf("settings = %+v, want configured and enabled", body)
	}

	if _, restarts := fake.counts(); len(restarts) != 1 || restarts[0] != store.ProviderDiscord {
		t.Fatalf("restarts = %v, want the new token to restart the session", restarts)
	}
}

// The one thing this endpoint must never do.
func TestTheTokenIsNeverReturned(t *testing.T) {
	e := newTestEnv(t)
	e.withSupervisor()

	const token = "MTIzNDU2Nzg5.abcdef.SECRETVALUE"
	save := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"token": token, "enabled": true}, e.adminToken)
	if save.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", save.StatusCode)
	}

	// Both the response to the save and the listing afterwards.
	for name, resp := range map[string]*http.Response{
		"the save":    save,
		"the listing": e.do(http.MethodGet, "/api/v1/admin/bots", nil, e.adminToken),
	} {
		raw, err := json.Marshal(decodeAny(t, resp))
		if err != nil {
			t.Fatalf("re-encoding %s: %v", name, err)
		}
		if strings.Contains(string(raw), token) {
			t.Fatalf("%s carries the token: %s", name, raw)
		}
		if strings.Contains(string(raw), "SECRET") {
			t.Fatalf("%s carries part of the token: %s", name, raw)
		}
	}

	// The hint is the last four characters and nothing more.
	listing := decodeJSON[listResponse[botResponse]](t,
		e.do(http.MethodGet, "/api/v1/admin/bots", nil, e.adminToken))
	for _, item := range listing.Items {
		if item.Provider != store.ProviderDiscord {
			continue
		}
		shown := strings.TrimPrefix(item.TokenHint, "…")
		if !strings.HasSuffix(token, shown) {
			t.Errorf("hint = %q, which is not the end of the token", item.TokenHint)
		}
		if len(shown) > 4 {
			t.Errorf("hint = %q shows %d characters, want at most 4", item.TokenHint, len(shown))
		}
	}
}

// Flipping the switch must not require pasting the secret again.
func TestTheSwitchWorksWithoutTheToken(t *testing.T) {
	e := newTestEnv(t)
	fake := e.withSupervisor()

	if resp := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"token": "MTIzNDU2Nzg5.abcdef.ghijkl", "enabled": true}, e.adminToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("saving: %d", resp.StatusCode)
	}
	beforeSyncs, beforeRestarts := fake.counts()

	resp := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"enabled": false}, e.adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[botResponse](t, resp)
	if body.Enabled {
		t.Error("the switch did not turn off")
	}
	if !body.Configured {
		t.Error("turning the switch off forgot the token")
	}

	// A save that did not change the token syncs rather than restarts: a
	// restart would disconnect a working bot for no reason.
	syncs, restarts := fake.counts()
	if syncs != beforeSyncs+1 {
		t.Errorf("syncs = %d, want one more than %d", syncs, beforeSyncs)
	}
	if len(restarts) != len(beforeRestarts) {
		t.Errorf("restarts = %v, want no new ones", restarts)
	}
}

func TestSwitchingOnWithoutATokenIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.withSupervisor()

	resp := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"enabled": true}, e.adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAnEmptyTokenIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.withSupervisor()

	resp := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"token": "   "}, e.adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAnUnknownPlatformIsRefused(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPut, "/api/v1/admin/bots/irc",
		map[string]any{"token": "x", "enabled": true}, e.adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDeletingForgetsTheToken(t *testing.T) {
	e := newTestEnv(t)
	e.withSupervisor()

	if resp := e.do(http.MethodPut, "/api/v1/admin/bots/discord",
		map[string]any{"token": "MTIzNDU2Nzg5.abcdef.ghijkl", "enabled": true}, e.adminToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("saving: %d", resp.StatusCode)
	}

	resp := e.do(http.MethodDelete, "/api/v1/admin/bots/discord", nil, e.adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	listing := decodeJSON[listResponse[botResponse]](t,
		e.do(http.MethodGet, "/api/v1/admin/bots", nil, e.adminToken))
	for _, item := range listing.Items {
		if item.Provider == store.ProviderDiscord && item.Configured {
			t.Fatal("the token survived deletion")
		}
	}

	if resp := e.do(http.MethodDelete, "/api/v1/admin/bots/discord", nil, e.adminToken); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", resp.StatusCode)
	}
}

// A bot can act for everyone who linked their account, so configuring one is
// not something an ordinary user does.
func TestBotSettingsAreAdminOnly(t *testing.T) {
	e := newTestEnv(t)

	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/admin/bots", nil},
		{http.MethodPut, "/api/v1/admin/bots/discord", map[string]any{"token": "x"}},
		{http.MethodDelete, "/api/v1/admin/bots/discord", nil},
	} {
		resp := e.do(call.method, call.path, call.body, e.token)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a user = %d, want 403", call.method, call.path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// decodeAny reads a response into a generic structure, for assertions about
// what is in the JSON rather than about its shape.
func decodeAny(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return out
}
