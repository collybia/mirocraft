package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// LetsEncryptDirectory is the default certificate authority.
const LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// Timeouts for the parts of an issuance that can hang.
const (
	// orderTimeout bounds one whole issuance.
	orderTimeout = 5 * time.Minute
	// dnsPropagationPoll bounds waiting for the TXT record to become visible.
	//
	// Not optional politeness: a validator that checks too early sees the old
	// record, marks the challenge invalid, and the order cannot be retried —
	// a new one has to be started, which counts against the rate limit.
	dnsPropagationPoll = 3 * time.Minute
)

// fromACME obtains a certificate from a certificate authority.
func (m *Manager) fromACME(ctx context.Context) (*tls.Certificate, error) {
	// A stored certificate with weeks left is used as it is. Asking for a new
	// one on every restart is how a panel that restarts often runs into the
	// authority's rate limit and ends up with no certificate at all.
	if cert, ok := m.loadACMECert(); ok {
		m.log.Info("reusing the stored certificate",
			slog.Time("expires", cert.Leaf.NotAfter))
		return cert, nil
	}

	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()

	client, err := m.acmeClient(ctx)
	if err != nil {
		return nil, err
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(m.cfg.Domain))
	if err != nil {
		return nil, fmt.Errorf("certs: starting the order for %s: %w", m.cfg.Domain, err)
	}

	for _, authzURL := range order.AuthzURLs {
		if err := m.satisfy(ctx, client, authzURL); err != nil {
			return nil, err
		}
	}

	// Waited on, but the finalize URL is taken from the original order: it
	// does not change, and the order returned here does not always carry it.
	if _, err := client.WaitOrder(ctx, order.URI); err != nil {
		return nil, fmt.Errorf("certs: the authority did not authorize %s: %w", m.cfg.Domain, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generating the certificate key: %w", err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: m.cfg.Domain},
		DNSNames: []string{m.cfg.Domain},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("certs: building the request: %w", err)
	}

	chain, err := m.finalize(ctx, client, order, csr)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, fmt.Errorf("certs: parsing the issued certificate: %w", err)
	}

	cert := &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}
	if err := m.saveACMECert(cert); err != nil {
		m.log.Warn("saving the certificate failed; it will be requested again on restart",
			slog.String("error", err.Error()))
	}
	return cert, nil
}

// finalize sends the request and collects the issued certificate.
//
// CreateOrderCert does both, but it finds the order to poll in the Location
// header of the finalize response — which RFC 8555 does not require and some
// authorities do not send. When that header is missing it polls an empty URL
// and fails, having already submitted a request the authority went on to
// honour. So a failure there is not taken at face value: the order is polled
// at the URL we already have, and if a certificate did appear it is collected.
func (m *Manager) finalize(ctx context.Context, client *acme.Client, order *acme.Order, csr []byte) ([][]byte, error) {
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err == nil {
		return chain, nil
	}
	firstErr := err

	settled, waitErr := client.WaitOrder(ctx, order.URI)
	if waitErr != nil {
		return nil, fmt.Errorf("certs: collecting the certificate: %w (and the order did not settle: %v)",
			firstErr, waitErr)
	}
	if settled.CertURL == "" {
		return nil, fmt.Errorf("certs: collecting the certificate: %w", firstErr)
	}

	chain, err = client.FetchCert(ctx, settled.CertURL, true)
	if err != nil {
		return nil, fmt.Errorf("certs: fetching the issued certificate: %w", err)
	}
	m.log.Debug("the authority sent no order location on finalize; collected the certificate by polling")
	return chain, nil
}

// satisfy answers one authorization.
func (m *Manager) satisfy(ctx context.Context, client *acme.Client, authzURL string) error {
	authz, err := client.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("certs: reading the authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		// Already answered, usually from a recent order. Nothing to do, and
		// re-answering would fail.
		return nil
	}

	// The challenge type is a plain string in the protocol, and x/crypto/acme
	// exports no constants for it — the values here are the same ones.
	wanted := m.cfg.Challenge

	var challenge *acme.Challenge
	for _, candidate := range authz.Challenges {
		if candidate.Type == wanted {
			challenge = candidate
			break
		}
	}
	if challenge == nil {
		return fmt.Errorf("certs: the authority did not offer a %s challenge for %s",
			wanted, m.cfg.Domain)
	}

	cleanup, err := m.prepare(ctx, client, challenge)
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("certs: answering the %s challenge: %w", wanted, err)
	}
	if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
		return fmt.Errorf("certs: the %s challenge for %s was not accepted: %w",
			wanted, m.cfg.Domain, err)
	}
	return nil
}

// prepare puts the challenge answer where the authority will look for it, and
// returns a function that takes it away again.
func (m *Manager) prepare(ctx context.Context, client *acme.Client, challenge *acme.Challenge) (func(), error) {
	if challenge.Type == ChallengeHTTP01 {
		path := client.HTTP01ChallengePath(challenge.Token)
		body, err := client.HTTP01ChallengeResponse(challenge.Token)
		if err != nil {
			return nil, fmt.Errorf("certs: building the http-01 response: %w", err)
		}

		m.httpSolver.set(path, body)
		return func() { m.httpSolver.clear() }, nil
	}

	value, err := client.DNS01ChallengeRecord(challenge.Token)
	if err != nil {
		return nil, fmt.Errorf("certs: building the dns-01 record: %w", err)
	}

	// The label is relative to the zone the solver writes into, which may be
	// the domain itself or a name under it.
	sub := challengeSub(m.cfg.Domain, m.solver.Zone())
	if err := m.solver.EnsureTXT(ctx, sub, []string{value}); err != nil {
		return nil, fmt.Errorf("certs: publishing the dns-01 record: %w", err)
	}

	cleanup := func() {
		// A left-behind token is a standing invitation to anyone who can read
		// the zone, so it goes even when the challenge failed. Its own context:
		// the order's may already be cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := m.solver.DeleteTXT(cleanupCtx, sub); err != nil {
			m.log.Warn("removing the dns-01 record failed",
				slog.String("record", sub), slog.String("error", err.Error()))
		}
	}

	name := sub
	if m.solver.Zone() != "" {
		name = strings.TrimSuffix(sub+"."+m.solver.Zone(), ".")
	}
	m.awaitPropagation(ctx, name, value)
	return cleanup, nil
}

// challengeSub is the label to write the token at, relative to the solver's
// zone.
//
// The domain and the zone can differ: a certificate for mc.example.com issued
// through a provider driving example.com needs _acme-challenge.mc, not
// _acme-challenge.
func challengeSub(domain, zone string) string {
	label := strings.TrimSuffix(strings.ToLower(domain), ".")
	zone = strings.TrimSuffix(strings.ToLower(zone), ".")

	if zone != "" && label != zone && strings.HasSuffix(label, "."+zone) {
		return acmeChallengeLabel + "." + strings.TrimSuffix(label, "."+zone)
	}
	return acmeChallengeLabel
}

// acmeChallengeLabel is the name ACME looks up. Duplicated from the dns
// package rather than imported: this package has no other reason to depend on
// it, and the string is fixed by the protocol.
const acmeChallengeLabel = "_acme-challenge"

// awaitPropagation waits until this host's own resolver can see the record.
//
// Not a guarantee — the authority queries the zone's authoritative servers,
// not ours — but it is the best signal available, and it turns the usual
// failure ("the validator looked too early") into a wait.
func (m *Manager) awaitPropagation(ctx context.Context, name, want string) {
	deadline := time.Now().Add(dnsPropagationPoll)
	resolver := m.cfg.Resolver
	if resolver == nil {
		resolver = &net.Resolver{}
	}

	for time.Now().Before(deadline) {
		values, err := resolver.LookupTXT(ctx, name)
		if err == nil {
			for _, value := range values {
				if value == want {
					// Even once visible, a moment's grace: the authority may
					// ask a server that has not caught up.
					select {
					case <-ctx.Done():
					case <-time.After(2 * time.Second):
					}
					return
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	m.log.Warn("the dns-01 record is not visible from here yet; asking the authority anyway",
		slog.String("record", name), slog.Duration("waited", dnsPropagationPoll))
}

// acmeClient builds a client, reusing the stored account key.
//
// The key is the account: losing it means the authority no longer recognises
// this panel, and the rate limits that apply to a new account apply again.
func (m *Manager) acmeClient(ctx context.Context) (*acme.Client, error) {
	key, err := m.accountKey()
	if err != nil {
		return nil, err
	}

	directory := m.cfg.DirectoryURL
	if directory == "" {
		directory = LetsEncryptDirectory
	}

	client := &acme.Client{
		Key:          key,
		DirectoryURL: directory,
		UserAgent:    "mirocraft",
		HTTPClient:   m.cfg.HTTPClient,
	}

	account := &acme.Account{}
	if m.cfg.Email != "" {
		account.Contact = []string{"mailto:" + m.cfg.Email}
	}

	// An account this key already registered is not an error the caller needs
	// to see: the authority answers 409 and the existing account is the one
	// that will be used.
	if _, err := client.Register(ctx, account, acme.AcceptTOS); err != nil {
		var apiErr *acme.Error
		alreadyRegistered := errors.Is(err, acme.ErrAccountAlreadyExists) ||
			(errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict)
		if !alreadyRegistered {
			return nil, fmt.Errorf("certs: registering with the authority: %w", err)
		}
	}
	return client, nil
}

// accountKey loads the ACME account key, generating one on first use.
func (m *Manager) accountKey() (*ecdsa.PrivateKey, error) {
	path := filepath.Join(m.cfg.Dir, "acme-account.key")

	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block != nil {
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err == nil {
				if key, ok := parsed.(*ecdsa.PrivateKey); ok {
					return key, nil
				}
			}
		}
		m.log.Warn("the stored ACME account key is unreadable; generating a new account",
			slog.String("path", path))
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generating an account key: %w", err)
	}

	if err := os.MkdirAll(m.cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("certs: creating %s: %w", m.cfg.Dir, err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("certs: encoding the account key: %w", err)
	}
	if err := os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("certs: writing the account key: %w", err)
	}
	return key, nil
}

// saveACMECert stores the issued certificate so a restart does not ask for a
// new one — which would count against the authority's rate limit.
func (m *Manager) saveACMECert(cert *tls.Certificate) error {
	return savePair(m.cfg.Dir,
		filepath.Join(m.cfg.Dir, "acme.crt"),
		filepath.Join(m.cfg.Dir, "acme.key"),
		cert)
}

// loadACMECert returns a stored certificate that is still worth serving.
func (m *Manager) loadACMECert() (*tls.Certificate, bool) {
	cert, err := loadPair(
		filepath.Join(m.cfg.Dir, "acme.crt"),
		filepath.Join(m.cfg.Dir, "acme.key"))
	if err != nil {
		return nil, false
	}
	if time.Until(cert.Leaf.NotAfter) < renewBefore(cert.Leaf.NotBefore, cert.Leaf.NotAfter) {
		return nil, false
	}
	// A stored certificate for a different name is no use, and serving it
	// would produce a browser warning that looks like a panel bug.
	if err := cert.Leaf.VerifyHostname(m.cfg.Domain); err != nil {
		return nil, false
	}
	return cert, true
}

// httpSolver serves the HTTP-01 challenge response.
//
// A tiny handler rather than autocert: autocert owns the whole certificate
// lifecycle and cannot do DNS-01, and running two different mechanisms
// depending on the challenge would double the code that decides what is
// current.
type httpSolver struct {
	mu   sync.RWMutex
	path string
	body string
}

func (s *httpSolver) set(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.body = path, body
}

func (s *httpSolver) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.body = "", ""
}

// ServeHTTP answers the challenge, and nothing else.
func (s *httpSolver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	path, body := s.path, s.body
	s.mu.RUnlock()

	if path == "" || r.URL.Path != path {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(body))
}

// HTTPChallengeHandler returns the handler that answers HTTP-01, or nil when
// the mode does not need one.
//
// Mounted by the daemon on port 80: the challenge is fetched over plain HTTP
// by definition, so it cannot be served by the HTTPS listener it exists to
// obtain a certificate for.
func (m *Manager) HTTPChallengeHandler() http.Handler {
	if m.cfg.Mode != ModeACME || m.cfg.Challenge != ChallengeHTTP01 {
		return nil
	}
	return m.httpSolver
}
