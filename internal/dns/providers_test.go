package dns

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

var (
	testV4 = netip.MustParseAddr("203.0.113.7")
	testV6 = netip.MustParseAddr("2001:db8::1")
)

// --- deSEC ---

type deSECStub struct {
	*httptest.Server
	sets   []rrset
	status int
	body   string
	method string
	path   string
	auth   string
}

func newDeSECStub(t *testing.T) *deSECStub {
	t.Helper()
	stub := &deSECStub{}

	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.method, stub.path = r.Method, r.URL.Path
		stub.auth = r.Header.Get("Authorization")

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &stub.sets)

		if stub.status != 0 {
			w.WriteHeader(stub.status)
			_, _ = io.WriteString(w, stub.body)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "[]")
	}))

	t.Cleanup(stub.Close)
	return stub
}

func (s *deSECStub) provider(t *testing.T, cfg Config) *DeSEC {
	t.Helper()
	if cfg.Zone == "" {
		cfg.Zone = "myname.dedyn.io"
	}
	if cfg.Token == "" {
		cfg.Token = "secret-token"
	}

	provider, err := NewDeSEC(cfg, s.Server.Client())
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	provider.BaseURL = s.Server.URL
	return provider
}

func TestDeSECPublishesAnAddress(t *testing.T) {
	stub := newDeSECStub(t)
	provider := stub.provider(t, Config{})

	if err := provider.EnsureAddress(context.Background(), "mc", testV4); err != nil {
		t.Fatalf("EnsureAddress: %v", err)
	}

	// PUT rather than POST: a dynamic-DNS updater runs every few minutes, and
	// POST would fail the second time with "already exists".
	if stub.method != http.MethodPut {
		t.Errorf("method = %s, want PUT so repeated updates are idempotent", stub.method)
	}
	if !strings.Contains(stub.path, "/domains/myname.dedyn.io/rrsets/") {
		t.Errorf("path = %s", stub.path)
	}
	if stub.auth != "Token secret-token" {
		t.Errorf("authorization = %q", stub.auth)
	}

	if len(stub.sets) != 1 {
		t.Fatalf("sets = %+v", stub.sets)
	}
	got := stub.sets[0]
	if got.Subname != "mc" || got.Type != "A" || got.Records[0] != "203.0.113.7" {
		t.Errorf("set = %+v", got)
	}
	// deSEC refuses anything under an hour, and asking for less gets the whole
	// request rejected rather than rounded.
	if got.TTL < DeSECMinTTL {
		t.Errorf("ttl = %d, below the deSEC floor of %d", got.TTL, DeSECMinTTL)
	}
}

func TestDeSECPublishesAAAAForV6(t *testing.T) {
	stub := newDeSECStub(t)
	provider := stub.provider(t, Config{})

	if err := provider.EnsureAddress(context.Background(), "", testV6); err != nil {
		t.Fatalf("EnsureAddress: %v", err)
	}
	if stub.sets[0].Type != "AAAA" {
		t.Fatalf("type = %q for a v6 address", stub.sets[0].Type)
	}
}

func TestDeSECPublishesSRV(t *testing.T) {
	stub := newDeSECStub(t)
	provider := stub.provider(t, Config{})

	if err := provider.EnsureSRV(context.Background(), "mc", "mc.myname.dedyn.io", 25566); err != nil {
		t.Fatalf("EnsureSRV: %v", err)
	}

	got := stub.sets[0]
	if got.Subname != "_minecraft._tcp.mc" {
		t.Errorf("subname = %q, want the name a Java client looks up", got.Subname)
	}
	if got.Records[0] != "0 5 25566 mc.myname.dedyn.io." {
		t.Errorf("record = %q", got.Records[0])
	}
}

// deSEC deletes a set by writing it empty, which also covers a set that was
// never there — so cleaning up twice is not an error.
func TestDeSECCleanupWritesEmptySets(t *testing.T) {
	stub := newDeSECStub(t)
	provider := stub.provider(t, Config{})

	if err := provider.Cleanup(context.Background(), "mc"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	types := map[string]bool{}
	for _, set := range stub.sets {
		types[set.Type] = true
		if len(set.Records) != 0 {
			t.Errorf("%s was not cleared: %+v", set.Type, set.Records)
		}
	}
	for _, want := range []string{"A", "AAAA", "SRV"} {
		if !types[want] {
			t.Errorf("cleanup left %s behind", want)
		}
	}
}

func TestDeSECReportsAuthFailure(t *testing.T) {
	stub := newDeSECStub(t)
	stub.status = http.StatusUnauthorized
	provider := stub.provider(t, Config{})

	err := provider.EnsureAddress(context.Background(), "mc", testV4)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("error = %v, want ErrAuth so the installer can say the token is wrong", err)
	}
}

// The body carries the field-level reason; dropping it turns "ttl must be at
// least 3600" into "400".
func TestDeSECKeepsTheUpstreamReason(t *testing.T) {
	stub := newDeSECStub(t)
	stub.status = http.StatusBadRequest
	stub.body = `{"ttl":["Ensure this value is greater than or equal to 3600."]}`
	provider := stub.provider(t, Config{})

	err := provider.EnsureAddress(context.Background(), "mc", testV4)
	if err == nil || !strings.Contains(err.Error(), "3600") {
		t.Fatalf("error = %v, want the upstream reason kept", err)
	}
}

// --- DuckDNS ---

type duckStub struct {
	*httptest.Server
	query  url.Values
	status int
	body   string
}

func newDuckStub(t *testing.T, body string) *duckStub {
	t.Helper()
	stub := &duckStub{body: body}

	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.query = r.URL.Query()
		if stub.status != 0 {
			w.WriteHeader(stub.status)
		}
		_, _ = io.WriteString(w, stub.body)
	}))

	t.Cleanup(stub.Close)
	return stub
}

func (s *duckStub) provider(t *testing.T) *DuckDNS {
	t.Helper()
	provider, err := NewDuckDNS(Config{Zone: "myname", Token: "duck-token"}, s.Server.Client())
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	provider.BaseURL = s.Server.URL
	return provider
}

func TestDuckDNSPublishesAnAddress(t *testing.T) {
	stub := newDuckStub(t, "OK")
	provider := stub.provider(t)

	if err := provider.EnsureAddress(context.Background(), "", testV4); err != nil {
		t.Fatalf("EnsureAddress: %v", err)
	}

	if stub.query.Get("domains") != "myname" || stub.query.Get("token") != "duck-token" {
		t.Errorf("query = %v", stub.query)
	}
	if stub.query.Get("ip") != "203.0.113.7" {
		t.Errorf("ip = %q", stub.query.Get("ip"))
	}
}

// The families go in separate parameters; a v6 address in the v4 one is
// accepted and then does nothing.
func TestDuckDNSUsesTheIPv6Parameter(t *testing.T) {
	stub := newDuckStub(t, "OK")
	provider := stub.provider(t)

	if err := provider.EnsureAddress(context.Background(), "", testV6); err != nil {
		t.Fatalf("EnsureAddress: %v", err)
	}
	if stub.query.Get("ipv6") != "2001:db8::1" {
		t.Errorf("ipv6 = %q", stub.query.Get("ipv6"))
	}
	if stub.query.Get("ip") != "" {
		t.Errorf("a v6 address went into the ip parameter: %q", stub.query.Get("ip"))
	}
}

// DuckDNS answers 200 whatever happens and puts the outcome in the body.
// Trusting the status would report a wrong token as success, and the operator
// would be left wondering why the name never resolves.
func TestDuckDNSTreatsKOAsAFailure(t *testing.T) {
	stub := newDuckStub(t, "KO")
	provider := stub.provider(t)

	err := provider.EnsureAddress(context.Background(), "", testV4)
	if err == nil {
		t.Fatal("a KO response was treated as success")
	}
	if !errors.Is(err, ErrAuth) {
		t.Errorf("error = %v, want ErrAuth: KO almost always means the token", err)
	}
}

func TestDuckDNSRefusesSubdomainsAndSRV(t *testing.T) {
	stub := newDuckStub(t, "OK")
	provider := stub.provider(t)

	err := provider.EnsureAddress(context.Background(), "mc", testV4)
	if !IsUnsupported(err) {
		t.Errorf("a subdomain gave %v, want ErrUnsupported rather than a record on the apex", err)
	}

	err = provider.EnsureSRV(context.Background(), "", "myname.duckdns.org", 25566)
	if !IsUnsupported(err) {
		t.Errorf("SRV gave %v, want ErrUnsupported", err)
	}
}

// --- Cloudflare ---

type cfStub struct {
	*httptest.Server

	// records is the zone's contents, keyed by id.
	records map[string]cfRecord
	nextID  int
	calls   []string
	// failWith makes every call answer success:false, the way Cloudflare can
	// on a 200.
	failWith string
}

func newCFStub(t *testing.T) *cfStub {
	t.Helper()
	stub := &cfStub{records: map[string]cfRecord{}}

	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls = append(stub.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		if stub.failWith != "" {
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":1004,"message":"`+
				stub.failWith+`"}],"result":null}`)
			return
		}

		switch {
		case r.URL.Path == "/zones":
			_, _ = io.WriteString(w,
				`{"success":true,"errors":[],"result":[{"id":"zone-1","name":"example.com"}]}`)

		case r.URL.Path == "/zones/zone-1/dns_records" && r.Method == http.MethodGet:
			wanted := r.URL.Query()
			var matched []cfRecord
			for _, record := range stub.records {
				if record.Type == wanted.Get("type") && record.Name == wanted.Get("name") {
					matched = append(matched, record)
				}
			}
			payload, _ := json.Marshal(matched)
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":`+string(payload)+`}`)

		case r.URL.Path == "/zones/zone-1/dns_records" && r.Method == http.MethodPost:
			var record cfRecord
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &record)
			stub.nextID++
			record.ID = "rec-" + string(rune('a'+stub.nextID-1))
			stub.records[record.ID] = record
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":{}}`)

		case strings.HasPrefix(r.URL.Path, "/zones/zone-1/dns_records/") && r.Method == http.MethodPut:
			id := strings.TrimPrefix(r.URL.Path, "/zones/zone-1/dns_records/")
			var record cfRecord
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &record)
			record.ID = id
			stub.records[id] = record
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":{}}`)

		case strings.HasPrefix(r.URL.Path, "/zones/zone-1/dns_records/") && r.Method == http.MethodDelete:
			delete(stub.records, strings.TrimPrefix(r.URL.Path, "/zones/zone-1/dns_records/"))
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":{}}`)

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":7003,"message":"no route"}]}`)
		}
	}))

	t.Cleanup(stub.Close)
	return stub
}

func (s *cfStub) provider(t *testing.T) *Cloudflare {
	t.Helper()
	provider, err := NewCloudflare(Config{Zone: "example.com", Token: "cf-token"}, s.Server.Client())
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	provider.BaseURL = s.Server.URL
	return provider
}

func TestCloudflareCreatesThenUpdates(t *testing.T) {
	stub := newCFStub(t)
	provider := stub.provider(t)
	ctx := context.Background()

	if err := provider.EnsureAddress(ctx, "mc", testV4); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if len(stub.records) != 1 {
		t.Fatalf("records = %+v", stub.records)
	}

	// The second call must update rather than create: two A records for one
	// name means half the players get an address that stopped being right.
	if err := provider.EnsureAddress(ctx, "mc", netip.MustParseAddr("203.0.113.9")); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if len(stub.records) != 1 {
		t.Fatalf("a second record was created: %+v", stub.records)
	}
	for _, record := range stub.records {
		if record.Content != "203.0.113.9" {
			t.Errorf("content = %q, want the new address", record.Content)
		}
		// Cloudflare's proxy does not carry the Minecraft protocol, so a
		// proxied record points players at an edge where nothing is listening.
		if record.Proxied {
			t.Error("the record is proxied, which breaks Minecraft")
		}
	}
}

// Duplicates from an earlier run or a hand edit are served alongside the right
// one, so they are removed rather than left.
func TestCloudflareRemovesDuplicates(t *testing.T) {
	stub := newCFStub(t)
	stub.records["stale-1"] = cfRecord{ID: "stale-1", Type: "A", Name: "mc.example.com", Content: "198.51.100.1"}
	stub.records["stale-2"] = cfRecord{ID: "stale-2", Type: "A", Name: "mc.example.com", Content: "198.51.100.2"}

	provider := stub.provider(t)
	if err := provider.EnsureAddress(context.Background(), "mc", testV4); err != nil {
		t.Fatalf("EnsureAddress: %v", err)
	}

	if len(stub.records) != 1 {
		t.Fatalf("%d records remain: %+v", len(stub.records), stub.records)
	}
	for _, record := range stub.records {
		if record.Content != "203.0.113.7" {
			t.Errorf("the surviving record is %q", record.Content)
		}
	}
}

func TestCloudflarePublishesSRV(t *testing.T) {
	stub := newCFStub(t)
	provider := stub.provider(t)

	if err := provider.EnsureSRV(context.Background(), "mc", "mc.example.com", 25566); err != nil {
		t.Fatalf("EnsureSRV: %v", err)
	}

	for _, record := range stub.records {
		if record.Name != "_minecraft._tcp.mc.example.com" {
			t.Errorf("name = %q", record.Name)
		}
		if record.Content != "0 5 25566 mc.example.com." {
			t.Errorf("content = %q", record.Content)
		}
	}
}

func TestCloudflareCleanupRemovesEverything(t *testing.T) {
	stub := newCFStub(t)
	provider := stub.provider(t)
	ctx := context.Background()

	if err := provider.EnsureAddress(ctx, "mc", testV4); err != nil {
		t.Fatalf("EnsureAddress: %v", err)
	}
	if err := provider.EnsureSRV(ctx, "mc", "mc.example.com", 25566); err != nil {
		t.Fatalf("EnsureSRV: %v", err)
	}
	if err := provider.Cleanup(ctx, "mc"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if len(stub.records) != 0 {
		t.Fatalf("cleanup left %+v", stub.records)
	}
}

// Cloudflare can answer 200 with success:false, so the envelope decides. The
// status alone would report a refusal as a success.
func TestCloudflareHonoursTheEnvelope(t *testing.T) {
	stub := newCFStub(t)
	stub.failWith = "DNS Validation Error"
	provider := stub.provider(t)

	err := provider.EnsureAddress(context.Background(), "mc", testV4)
	if err == nil {
		t.Fatal("a success:false response was treated as success")
	}
	if !strings.Contains(err.Error(), "DNS Validation Error") {
		t.Errorf("error = %v, want the upstream message", err)
	}
}

// The zone id never changes, and looking it up on every update would double
// the requests for nothing.
func TestCloudflareLooksTheZoneUpOnce(t *testing.T) {
	stub := newCFStub(t)
	provider := stub.provider(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := provider.EnsureAddress(ctx, "mc", testV4); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	var lookups int
	for _, call := range stub.calls {
		if strings.HasSuffix(call, " /zones") {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("the zone was looked up %d times", lookups)
	}
}

func TestCloudflareReportsAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`)
	}))
	defer server.Close()

	provider, err := NewCloudflare(Config{Zone: "example.com", Token: "bad"}, server.Client())
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	provider.BaseURL = server.URL

	if err := provider.EnsureAddress(context.Background(), "mc", testV4); !errors.Is(err, ErrAuth) {
		t.Fatalf("error = %v, want ErrAuth", err)
	}
}

func TestCloudflareReportsAnInvisibleZone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":[]}`)
	}))
	defer server.Close()

	provider, err := NewCloudflare(Config{Zone: "example.com", Token: "scoped-elsewhere"}, server.Client())
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	provider.BaseURL = server.URL

	err = provider.EnsureAddress(context.Background(), "mc", testV4)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	// The likely cause is a token scoped to another zone, and saying so saves
	// an hour of looking at the wrong thing.
	if !strings.Contains(err.Error(), "scoped") {
		t.Errorf("error = %v, want it to name the likely cause", err)
	}
}
