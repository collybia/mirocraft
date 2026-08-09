package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/collybia/mirocraft/internal/dns"
	"github.com/collybia/mirocraft/internal/store"
)

// stubDNS records what the API asked it to publish.
type stubDNS struct {
	mu        sync.Mutex
	published []string
	removed   []string
	caps      dns.Capabilities
	failWith  error
}

func (s *stubDNS) Zone() string                   { return "example.com" }
func (s *stubDNS) Capabilities() dns.Capabilities { return s.caps }

func (s *stubDNS) EnsureSRV(_ context.Context, sub, target string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.published = append(s.published, sub+" -> "+target+":"+itoa(port))
	return nil
}

func (s *stubDNS) Cleanup(_ context.Context, sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, sub)
	return nil
}

func (s *stubDNS) records() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.published...), append([]string(nil), s.removed...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func withDNS(t *testing.T, env *testEnv, stub *stubDNS) {
	t.Helper()
	env.api.dns = stub
}

// --- the name a server gets ---

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"survival":        "survival",
		"Survival":        "survival",
		"My Server":       "my-server",
		"creative  world": "creative-world",
		"  spaced  ":      "spaced",
		"--dashes--":      "dashes",
		"mix3d-42":        "mix3d-42",
		// Nothing usable survives, and the caller falls back to the id rather
		// than publishing a record under a mangled name. Normal here: server
		// names in this project's own tests are Russian.
		"Выживание": "",
		"!!!":       "",
	}
	for name, want := range cases {
		if got := slugify(name); got != want {
			t.Errorf("slugify(%q) = %q, want %q", name, got, want)
		}
	}

	// A name long enough to exceed a DNS label must still produce a valid one.
	long := slugify(string(make([]byte, 0, 200)) + "aaaaaaaaaa" + string(repeat('b', 200)))
	if long != "" && dns.ValidateSub(long) != nil {
		t.Errorf("a long name produced the invalid label %q", long)
	}
}

func repeat(c byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = c
	}
	return out
}

// A name a player can be told, rather than an id nobody can read out loud.
func TestServerSubUsesTheName(t *testing.T) {
	env := newTestEnv(t)
	withDNS(t, env, &stubDNS{caps: dns.Capabilities{SRV: true}})

	sub := env.api.serverSub(context.Background(), env.serverRecord)
	if sub != "owned" {
		t.Fatalf("sub = %q, want the server's name", sub)
	}
}

// Two servers sharing a name would otherwise overwrite each other's records,
// and whichever published last would quietly make the other unreachable.
func TestServerSubDisambiguatesCollisions(t *testing.T) {
	env := newTestEnv(t)
	withDNS(t, env, &stubDNS{caps: dns.Capabilities{SRV: true}})
	ctx := context.Background()

	twin := &store.Server{
		ID: "01TWINSERVER0000000000", OwnerID: env.user.ID, Name: env.serverRecord.Name,
		Core: "paper", Version: "1.21.4", RAMMb: 1024, Port: 25599,
		Dir: "servers/01TWINSERVER0000000000",
	}
	if err := env.db.Servers.Create(ctx, twin); err != nil {
		t.Fatalf("creating the twin: %v", err)
	}

	first := env.api.serverSub(ctx, env.serverRecord)
	second := env.api.serverSub(ctx, twin)

	if first == second {
		t.Fatalf("both servers got the name %q", first)
	}
	if dns.ValidateSub(second) != nil {
		t.Errorf("the disambiguated name %q is not a valid label", second)
	}
}

// A server named in Cyrillic leaves nothing usable, so the id is the fallback.
func TestServerSubFallsBackToTheID(t *testing.T) {
	env := newTestEnv(t)
	withDNS(t, env, &stubDNS{caps: dns.Capabilities{SRV: true}})
	ctx := context.Background()

	cyrillic := &store.Server{
		ID: "01CYRILLIC0000000000AB", OwnerID: env.user.ID, Name: "Выживание",
		Core: "paper", Version: "1.21.4", RAMMb: 1024, Port: 25598,
		Dir: "servers/01CYRILLIC0000000000AB",
	}
	if err := env.db.Servers.Create(ctx, cyrillic); err != nil {
		t.Fatalf("creating the server: %v", err)
	}

	sub := env.api.serverSub(ctx, cyrillic)
	if dns.ValidateSub(sub) != nil {
		t.Fatalf("the fallback name %q is not a valid label", sub)
	}
}

// --- publishing ---

func TestCreatingAServerPublishesItsSRV(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubDNS{caps: dns.Capabilities{SRV: true, Subdomains: true}}
	withDNS(t, env, stub)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPost, "/api/v1/servers", map[string]any{
		"name": "publishme", "core": "paper", "version": "1.21.4",
		"ram_mb": 1024, "port": 25601, "eula_accepted": true,
	}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	_ = resp.Body.Close()

	published, _ := stub.records()
	if len(published) != 1 {
		t.Fatalf("published = %v", published)
	}
	if published[0] != "publishme -> publishme.example.com:25601" {
		t.Fatalf("published %q", published[0])
	}
}

// A provider being down must not stop a server being created: the record can
// be republished later, but a failed creation loses the operator's work.
func TestCreatingAServerSurvivesADNSFailure(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubDNS{caps: dns.Capabilities{SRV: true}, failWith: errNotReachable}
	withDNS(t, env, stub)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPost, "/api/v1/servers", map[string]any{
		"name": "resilient", "core": "paper", "version": "1.21.4",
		"ram_mb": 1024, "port": 25602, "eula_accepted": true,
	}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want the server created anyway", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A rename moves the record; leaving the old one would keep pointing players
// at a port that may now belong to a different server.
func TestRenamingAServerMovesItsRecord(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubDNS{caps: dns.Capabilities{SRV: true, Subdomains: true}}
	withDNS(t, env, stub)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		map[string]any{"name": "renamed"}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	published, removed := stub.records()
	if len(removed) != 1 || removed[0] != "owned" {
		t.Fatalf("removed = %v, want the old name taken down", removed)
	}
	if len(published) != 1 || published[0] != "renamed -> renamed.example.com:25565" {
		t.Fatalf("published = %v", published)
	}
}

func TestChangingThePortRepublishes(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubDNS{caps: dns.Capabilities{SRV: true, Subdomains: true}}
	withDNS(t, env, stub)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		map[string]any{"port": 25610}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	published, removed := stub.records()
	// The name did not change, so nothing is taken down — only the port moves.
	if len(removed) != 0 {
		t.Errorf("a port change removed %v", removed)
	}
	if len(published) != 1 || published[0] != "owned -> owned.example.com:25610" {
		t.Fatalf("published = %v", published)
	}
}

// PATCH did not accept a port at all, so the panel's own options tab offered
// the field, answered "saved" and changed nothing. The DNS work found it
// because the record never moved.
func TestPatchChangesThePort(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	resp := env.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		map[string]any{"port": 25611}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := decodeJSON[serverResponse](t, resp).Port; got != 25611 {
		t.Fatalf("the response reports port %d", got)
	}

	stored := decodeJSON[serverResponse](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID, nil, token))
	if stored.Port != 25611 {
		t.Fatalf("the stored port is %d, so the change was only reported", stored.Port)
	}
}

// Two servers on one port means the second to start fails to bind, and the
// panel would have let the operator arrange that.
func TestPatchRefusesAPortAnotherServerHolds(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	// otherServerID is seeded on 25566.
	resp := env.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		map[string]any{"port": 25566}, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A server keeping its own port must not be told the port is taken — by
// itself.
func TestPatchAcceptsTheServersOwnPort(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	resp := env.do(http.MethodPatch, "/api/v1/servers/"+testServerID,
		map[string]any{"port": 25565, "ram_mb": 2048}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the unchanged port accepted", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestDeletingAServerRemovesItsRecord(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubDNS{caps: dns.Capabilities{SRV: true, Subdomains: true}}
	withDNS(t, env, stub)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodDelete,
		"/api/v1/servers/"+testServerID+"?confirm=owned", nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	_, removed := stub.records()
	if len(removed) != 1 || removed[0] != "owned" {
		t.Fatalf("removed = %v", removed)
	}
}

// --- the status endpoint ---

func TestDNSStatusWhenDisabled(t *testing.T) {
	env := newTestEnv(t)

	body := decodeJSON[dnsStatusResponse](t,
		env.do(http.MethodGet, "/api/v1/dns", nil, env.token))
	if body.Enabled {
		t.Fatalf("status = %+v, want DNS reported as off", body)
	}
}

func TestDNSStatusReportsSRVSupport(t *testing.T) {
	env := newTestEnv(t)
	withDNS(t, env, &stubDNS{caps: dns.Capabilities{SRV: false}})

	body := decodeJSON[dnsStatusResponse](t,
		env.do(http.MethodGet, "/api/v1/dns", nil, env.token))

	if !body.Enabled || body.Zone != "example.com" {
		t.Fatalf("status = %+v", body)
	}
	// The panel uses this to tell players whether they must type the port.
	if body.SRV {
		t.Error("SRV was reported as supported by a provider that cannot")
	}
}

var errNotReachable = &dnsError{"provider unreachable"}

type dnsError struct{ msg string }

func (e *dnsError) Error() string { return e.msg }
