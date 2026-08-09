package dns

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ipStub serves one body as a public-address source.
func ipStub(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// --- discovering the public address ---

func TestPublicIPTakesTheMajority(t *testing.T) {
	honest1 := ipStub(t, "203.0.113.7\n", 0)
	honest2 := ipStub(t, "203.0.113.7", 0)
	// One source that is wrong, or that has been taken over, would otherwise
	// send every server on the panel to an address the operator does not own.
	liar := ipStub(t, "198.51.100.99", 0)

	addr, err := publicIPFrom(context.Background(), nil,
		[]string{liar.URL, honest1.URL, honest2.URL})
	if err != nil {
		t.Fatalf("PublicIP: %v", err)
	}
	if addr.String() != "203.0.113.7" {
		t.Fatalf("addr = %s, want the majority answer", addr)
	}
}

// Two sources, two answers: still usable — the record is corrected on the next
// tick — but the caller is told so it can log rather than silently pick a side.
func TestPublicIPReportsDisagreement(t *testing.T) {
	one := ipStub(t, "203.0.113.7", 0)
	two := ipStub(t, "198.51.100.99", 0)

	addr, err := publicIPFrom(context.Background(), nil, []string{one.URL, two.URL})
	if !addr.IsValid() {
		t.Fatal("no address was returned despite a source answering")
	}
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("error = %v, want the disagreement reported", err)
	}
}

func TestPublicIPSurvivesADeadSource(t *testing.T) {
	dead := ipStub(t, "gateway timeout", http.StatusBadGateway)
	alive1 := ipStub(t, "203.0.113.7", 0)
	alive2 := ipStub(t, "203.0.113.7", 0)

	addr, err := publicIPFrom(context.Background(), nil, []string{dead.URL, alive1.URL, alive2.URL})
	if err != nil {
		t.Fatalf("PublicIP: %v", err)
	}
	if addr.String() != "203.0.113.7" {
		t.Fatalf("addr = %s", addr)
	}
}

func TestPublicIPFailsWhenNoSourceAnswers(t *testing.T) {
	dead := ipStub(t, "", http.StatusInternalServerError)

	if _, err := publicIPFrom(context.Background(), nil, []string{dead.URL}); err == nil {
		t.Fatal("a failing source was treated as success")
	}
}

// A source that started serving a web page must be rejected rather than parsed
// into something that looks like an address.
func TestPublicIPRejectsNonAddresses(t *testing.T) {
	html := ipStub(t, "<!doctype html><html><body>Your IP is 203.0.113.7</body></html>", 0)

	if _, err := publicIPFrom(context.Background(), nil, []string{html.URL}); err == nil {
		t.Fatal("an HTML page was accepted as an address")
	}
}

// --- the watcher ---

// fakeProvider records what was published.
type fakeProvider struct {
	mu        sync.Mutex
	addresses []netip.Addr
	srv       []string
	failWith  error
	caps      Capabilities
}

func (f *fakeProvider) ID() string                 { return "fake" }
func (f *fakeProvider) Name() string               { return "Fake" }
func (f *fakeProvider) Zone() string               { return "example.com" }
func (f *fakeProvider) Capabilities() Capabilities { return f.caps }

func (f *fakeProvider) EnsureAddress(_ context.Context, _ string, ip netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.addresses = append(f.addresses, ip)
	return nil
}

func (f *fakeProvider) EnsureSRV(_ context.Context, sub, target string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.caps.SRV {
		return ErrUnsupported
	}
	f.srv = append(f.srv, SRVValue(target, port)+" for "+sub)
	return nil
}

func (f *fakeProvider) Cleanup(context.Context, string) error { return nil }

func (f *fakeProvider) published() []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netip.Addr(nil), f.addresses...)
}

func TestWatcherPublishesOnceAndThenOnlyOnChange(t *testing.T) {
	provider := &fakeProvider{caps: Capabilities{SRV: true, Subdomains: true}}

	current := netip.MustParseAddr("203.0.113.7")
	watcher := NewWatcher(provider, "mc", silent())
	watcher.Lookup = func(context.Context) (netip.Addr, error) { return current, nil }

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := watcher.Check(ctx); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}

	// Rewriting the same record every five minutes is exactly the traffic
	// these free services ask people not to generate.
	if got := provider.published(); len(got) != 1 {
		t.Fatalf("an unchanged address was published %d times", len(got))
	}

	current = netip.MustParseAddr("203.0.113.9")
	if err := watcher.Check(ctx); err != nil {
		t.Fatalf("after the change: %v", err)
	}

	published := provider.published()
	if len(published) != 2 || published[1] != current {
		t.Fatalf("published = %v, want the new address", published)
	}
}

// A name that stopped updating looks exactly like a server that is down, so
// the failure has to be visible from the panel rather than only in the log.
func TestWatcherStatusCarriesTheLastFailure(t *testing.T) {
	provider := &fakeProvider{failWith: errors.New("the token expired")}

	watcher := NewWatcher(provider, "mc", silent())
	watcher.Lookup = func(context.Context) (netip.Addr, error) {
		return netip.MustParseAddr("203.0.113.7"), nil
	}

	if err := watcher.Check(context.Background()); err == nil {
		t.Fatal("a failing provider was treated as success")
	}

	status := watcher.Status()
	if status.Name != "mc.example.com" {
		t.Errorf("name = %q", status.Name)
	}
	if !strings.Contains(status.Error, "token expired") {
		t.Errorf("status error = %q", status.Error)
	}
	if status.CheckedAt.IsZero() {
		t.Error("the check time was not recorded")
	}
}

// A failure must not leave the watcher believing the record is current: the
// next check has to try again even if the address has not moved.
func TestWatcherRetriesAfterAFailure(t *testing.T) {
	provider := &fakeProvider{failWith: errors.New("upstream down")}

	watcher := NewWatcher(provider, "mc", silent())
	watcher.Lookup = func(context.Context) (netip.Addr, error) {
		return netip.MustParseAddr("203.0.113.7"), nil
	}

	ctx := context.Background()
	if err := watcher.Check(ctx); err == nil {
		t.Fatal("the first check should have failed")
	}

	provider.mu.Lock()
	provider.failWith = nil
	provider.mu.Unlock()

	if err := watcher.Check(ctx); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if len(provider.published()) != 1 {
		t.Fatal("the watcher did not retry after a failure")
	}
}

// A disagreement still yields a usable address, so it is logged and used
// rather than treated as a failure that leaves the name stale.
func TestWatcherUsesADisagreedAddress(t *testing.T) {
	provider := &fakeProvider{}

	watcher := NewWatcher(provider, "mc", silent())
	watcher.Lookup = func(context.Context) (netip.Addr, error) {
		return netip.MustParseAddr("203.0.113.7"), errors.New("dns: sources disagree")
	}

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(provider.published()) != 1 {
		t.Fatal("a disagreed address was not published")
	}
}

func TestWatcherRunStopsWithTheContext(t *testing.T) {
	provider := &fakeProvider{}
	watcher := NewWatcher(provider, "mc", silent())
	watcher.Interval = 10 * time.Millisecond
	watcher.Lookup = func(context.Context) (netip.Addr, error) {
		return netip.MustParseAddr("203.0.113.7"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
}

// --- SRV for one server ---

func TestEnsureServerSRV(t *testing.T) {
	provider := &fakeProvider{caps: Capabilities{SRV: true, Subdomains: true}}

	if err := EnsureServerSRV(context.Background(), provider, "mc", 25566); err != nil {
		t.Fatalf("EnsureServerSRV: %v", err)
	}
	if len(provider.srv) != 1 || !strings.Contains(provider.srv[0], "25566 mc.example.com.") {
		t.Fatalf("srv = %v", provider.srv)
	}
}

// A provider that cannot publish SRV is not an error: the caller degrades and
// explains that players will have to type the port.
func TestEnsureServerSRVReportsUnsupported(t *testing.T) {
	provider := &fakeProvider{caps: Capabilities{SRV: false}}

	err := EnsureServerSRV(context.Background(), provider, "mc", 25566)
	if !IsUnsupported(err) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

// --- against the real internet ---

// The one part of this package that can be checked live without anyone's
// credentials, and the part most likely to rot: these services change their
// response format, add rate limits or disappear.
func TestLivePublicIP(t *testing.T) {
	if os.Getenv("MIROCRAFT_LIVE") == "" {
		t.Skip("set MIROCRAFT_LIVE=1 to query the real address services")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	addr, err := PublicIP(ctx, nil)
	if err != nil {
		// A disagreement still returns an address; only a total failure is
		// worth failing the test over.
		if !addr.IsValid() {
			t.Fatalf("no source could report the public address: %v", err)
		}
		t.Logf("sources disagreed: %v", err)
	}

	if !addr.IsValid() {
		t.Fatal("the address is not valid")
	}
	// A private address means a source answered with what it saw locally,
	// which would publish a record no player outside the network can use.
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		t.Fatalf("the reported public address is %s, which is not public", addr)
	}
	t.Logf("public address: %s", addr)
}

// Each source individually, so a broken one is named rather than hidden by the
// majority.
func TestLiveEachSourceAnswers(t *testing.T) {
	if os.Getenv("MIROCRAFT_LIVE") == "" {
		t.Skip("set MIROCRAFT_LIVE=1 to query the real address services")
	}

	for _, source := range PublicIPSources {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		addr, err := fetchIP(ctx, nil, source)
		cancel()

		if err != nil {
			t.Errorf("%s: %v", source, err)
			continue
		}
		t.Logf("%-32s %s", source, addr)
	}
}
