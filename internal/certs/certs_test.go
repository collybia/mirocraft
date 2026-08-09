package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newManager(t *testing.T, cfg Config, solver DNSSolver) *Manager {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}

	m, err := New(cfg, solver, silent())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// --- configuration ---

func TestNewRefusesConfigurationThatCannotWork(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name   string
		cfg    Config
		solver DNSSolver
	}{
		{"unknown mode", Config{Mode: "magic", Dir: dir}, nil},
		{"acme without a domain", Config{Mode: ModeACME, Dir: dir, AcceptTOS: true}, nil},
		{"acme without a directory", Config{Mode: ModeACME, Domain: "example.com", AcceptTOS: true}, nil},
		// Agreeing to someone else's terms on their behalf is putting words in
		// their mouth.
		{"acme without accepting the terms",
			Config{Mode: ModeACME, Domain: "example.com", Dir: dir}, nil},
		{"unknown challenge",
			Config{Mode: ModeACME, Domain: "example.com", Dir: dir, AcceptTOS: true, Challenge: "magic-01"}, nil},
		// The whole point of the DNS-01 path is the DNS provider; without one
		// it would fail at the challenge instead of at startup.
		{"dns-01 without a solver",
			Config{Mode: ModeACME, Domain: "example.com", Dir: dir, AcceptTOS: true, Challenge: ChallengeDNS01}, nil},
		{"self-signed without a directory", Config{Mode: ModeSelfSigned}, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg, c.solver, silent()); err == nil {
				t.Fatalf("New accepted %+v", c.cfg)
			} else if !errors.Is(err, ErrNotConfigured) {
				t.Errorf("error = %v, want ErrNotConfigured", err)
			}
		})
	}
}

func TestOffModeServesNothing(t *testing.T) {
	m := newManager(t, Config{Mode: ModeOff}, nil)

	if m.Enabled() {
		t.Error("the off mode reports itself enabled")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.Status().Trusted {
		t.Error("the off mode claims a trusted certificate")
	}
}

// --- self-signed ---

func TestSelfSignedIsUsable(t *testing.T) {
	dir := t.TempDir()
	m := newManager(t, Config{Mode: ModeSelfSigned, Domain: "panel.example.com", Dir: dir}, nil)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	status := m.Status()
	// The panel says so plainly rather than leaving the browser to explain.
	if status.Trusted {
		t.Error("a self-signed certificate is reported as trusted")
	}
	if status.NotAfter.Before(time.Now().Add(300 * 24 * time.Hour)) {
		t.Errorf("expires %s, sooner than expected", status.NotAfter)
	}

	cert, err := m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if err := cert.Leaf.VerifyHostname("panel.example.com"); err != nil {
		t.Errorf("the certificate does not cover its own domain: %v", err)
	}

	// Both files on disk, and the key not readable by the world.
	for _, name := range []string{selfSignedCertName, selfSignedKeyName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// A browser told to trust this exception, or an operator who checked the
// fingerprint, should not have to do it again on every restart.
func TestSelfSignedIsReusedAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	first := newManager(t, Config{Mode: ModeSelfSigned, Domain: "panel.example.com", Dir: dir}, nil)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	firstSerial := first.Status().NotAfter

	second := newManager(t, Config{Mode: ModeSelfSigned, Domain: "panel.example.com", Dir: dir}, nil)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if !second.Status().NotAfter.Equal(firstSerial) {
		t.Fatal("the certificate was regenerated on restart")
	}
}

// The private key must not be readable by anyone else on the host. This is
// asserted here rather than left to the linter: the project writes 0640 files
// on purpose, so the mode checks are switched off in .golangci.yml, and the
// one file where the mode is the whole point needs its own guard.
func TestSelfSignedKeyIsNotReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows reports a synthesised mode; the real protection there is
		// the ACL the installer sets on the configuration directory.
		t.Skip("file modes are not meaningful on Windows")
	}

	dir := t.TempDir()
	m := newManager(t, Config{Mode: ModeSelfSigned, Domain: "panel.example.com", Dir: dir}, nil)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, selfSignedKeyName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("key mode = %#o, want no group or world bits", mode)
	}
}

// With no domain the panel is reached by address, so a name-only certificate
// would be rejected for exactly the way it is going to be used.
func TestSelfSignedWithoutADomainCoversAddresses(t *testing.T) {
	m := newManager(t, Config{Mode: ModeSelfSigned, Dir: t.TempDir()}, nil)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cert, err := m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	var hasLoopback bool
	for _, ip := range cert.Leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Errorf("the certificate covers no loopback address: %v", cert.Leaf.IPAddresses)
	}
}

// The generated certificate has to work in a real handshake, not merely parse.
func TestSelfSignedServesARealHandshake(t *testing.T) {
	m := newManager(t, Config{Mode: ModeSelfSigned, Domain: "panel.example.com", Dir: t.TempDir()}, nil)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello over tls")
	}))
	server.TLS = m.TLSConfig()
	server.StartTLS()
	defer server.Close()

	// The manager's own certificate, not server.Certificate(): httptest keeps
	// one of its own alongside, and trusting that would prove nothing about
	// what this package generated.
	served, err := m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	// Self-signed, so a client that verifies will refuse it — which is the
	// point. Trusting it explicitly is what an operator's browser exception
	// amounts to.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    poolWith(served.Leaf),
		ServerName: "panel.example.com",
		MinVersion: tls.VersionTLS12,
	}}}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello over tls" {
		t.Fatalf("body = %q", body)
	}
}

// A certificate near expiry is replaced rather than served until it dies.
func TestNeedsRenewal(t *testing.T) {
	m := newManager(t, Config{Mode: ModeSelfSigned, Dir: t.TempDir()}, nil)

	if !m.needsRenewal() {
		t.Error("a manager with no certificate does not want one")
	}

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.needsRenewal() {
		t.Error("a fresh certificate is already up for renewal")
	}

	// Pretend it is nearly expired. Both ends move: the window is a fraction
	// of the whole lifetime, so shortening only the end would make it look
	// like a freshly issued short-lived certificate rather than an old one.
	m.mu.Lock()
	leaf := m.current.Leaf
	leaf.NotBefore = time.Now().Add(-350 * 24 * time.Hour)
	leaf.NotAfter = time.Now().Add(15 * 24 * time.Hour)
	m.mu.Unlock()

	if !m.needsRenewal() {
		t.Error("a certificate expiring inside the renewal window is not renewed")
	}
}

// The renewal window follows the certificate's own lifetime.
//
// Authorities have started issuing certificates that last days rather than
// months. A fixed thirty-day threshold would make every one of those due for
// renewal the moment it was issued, so a panel that restarts twice orders
// twice and the third order meets the rate limit with nothing to show for it.
func TestRenewBeforeScalesWithLifetime(t *testing.T) {
	day := 24 * time.Hour
	now := time.Now()

	cases := []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
	}{
		// The familiar case: a third of ninety days is the usual thirty.
		{"ninety days", 90 * day, 30 * day},
		// Longer certificates do not get an ever-earlier renewal.
		{"a year", 365 * day, MaxRenewBefore},
		// The short-lived profile: two days of slack, not thirty.
		{"six days", 6 * day, 2 * day},
		// And nothing so short that a single failure is fatal.
		{"one day", day, MinRenewBefore},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renewBefore(now, now.Add(c.lifetime)); got != c.want {
				t.Errorf("renewBefore for a %s certificate = %s, want %s", c.lifetime, got, c.want)
			}
		})
	}

	// Nonsense dates must not produce a negative window, which would mean
	// never renewing.
	if got := renewBefore(now, now.Add(-time.Hour)); got <= 0 {
		t.Errorf("an already-expired certificate gives a window of %s", got)
	}
}

// --- the challenge plumbing ---

func TestChallengeSub(t *testing.T) {
	cases := []struct{ domain, zone, want string }{
		// The certificate is for the zone itself.
		{"example.com", "example.com", "_acme-challenge"},
		// A name under a zone the provider drives: the token goes beneath it,
		// not at the apex, or the validator looks in the wrong place.
		{"mc.example.com", "example.com", "_acme-challenge.mc"},
		{"a.b.example.com", "example.com", "_acme-challenge.a.b"},
		// A free subdomain, where the zone is the whole name.
		{"myname.dedyn.io", "myname.dedyn.io", "_acme-challenge"},
		// No zone known: the apex is the only sensible guess.
		{"example.com", "", "_acme-challenge"},
	}

	for _, c := range cases {
		if got := challengeSub(c.domain, c.zone); got != c.want {
			t.Errorf("challengeSub(%q, %q) = %q, want %q", c.domain, c.zone, got, c.want)
		}
	}
}

func TestHTTPChallengeHandlerOnlyExistsWhenNeeded(t *testing.T) {
	dns := newManager(t, Config{
		Mode: ModeACME, Domain: "example.com", Dir: t.TempDir(),
		AcceptTOS: true, Challenge: ChallengeDNS01,
	}, stubSolver{zone: "example.com"})
	if dns.HTTPChallengeHandler() != nil {
		t.Error("the dns-01 mode offers an HTTP challenge handler")
	}

	selfSigned := newManager(t, Config{Mode: ModeSelfSigned, Dir: t.TempDir()}, nil)
	if selfSigned.HTTPChallengeHandler() != nil {
		t.Error("the self-signed mode offers an HTTP challenge handler")
	}

	httpMode := newManager(t, Config{
		Mode: ModeACME, Domain: "example.com", Dir: t.TempDir(), AcceptTOS: true,
	}, nil)
	if httpMode.HTTPChallengeHandler() == nil {
		t.Fatal("the http-01 mode offers no handler, so the challenge cannot be answered")
	}
}

// The handler must answer the token it was given and nothing else: it sits on
// port 80, unauthenticated, and anything it serves is public.
func TestHTTPChallengeHandlerServesOnlyTheToken(t *testing.T) {
	m := newManager(t, Config{
		Mode: ModeACME, Domain: "example.com", Dir: t.TempDir(), AcceptTOS: true,
	}, nil)

	handler := m.HTTPChallengeHandler()
	m.httpSolver.set("/.well-known/acme-challenge/tok", "tok.keyauth")

	answered := httptest.NewRecorder()
	handler.ServeHTTP(answered, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok", nil))
	if answered.Body.String() != "tok.keyauth" {
		t.Errorf("the challenge answer = %q", answered.Body.String())
	}

	for _, path := range []string{"/", "/.well-known/acme-challenge/other", "/etc/passwd"} {
		other := httptest.NewRecorder()
		handler.ServeHTTP(other, httptest.NewRequest(http.MethodGet, path, nil))
		if other.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, other.Code)
		}
	}

	// Once the challenge is over the answer goes: it is a secret with no
	// further use.
	m.httpSolver.clear()
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok", nil))
	if after.Code != http.StatusNotFound {
		t.Errorf("the token is still served after the challenge: %d", after.Code)
	}
}

// --- helpers ---

type stubSolver struct {
	zone string
	txt  map[string][]string
}

func (s stubSolver) Zone() string { return s.zone }

func (s stubSolver) EnsureTXT(_ context.Context, sub string, values []string) error {
	if s.txt != nil {
		s.txt[sub] = values
	}
	return nil
}

func (s stubSolver) DeleteTXT(_ context.Context, sub string) error {
	if s.txt != nil {
		delete(s.txt, sub)
	}
	return nil
}

func poolWith(cert *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func TestStatusReportsTheLastFailure(t *testing.T) {
	m := newManager(t, Config{Mode: ModeSelfSigned, Dir: t.TempDir()}, nil)

	m.mu.Lock()
	m.lastErr = errors.New("the authority refused the order")
	m.mu.Unlock()

	if !strings.Contains(m.Status().Error, "refused the order") {
		t.Fatalf("status = %+v", m.Status())
	}
}
