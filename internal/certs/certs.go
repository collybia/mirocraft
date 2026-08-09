// Package certs obtains and renews the certificate the panel serves HTTPS
// with.
//
// Three ways, because an operator can be in three situations. With a domain
// and port 80 reachable, ACME's HTTP-01 challenge needs nothing else. With a
// domain but no reachable port 80 — behind a home router, or on a provider
// that blocks it — the DNS-01 challenge goes through the panel's own DNS
// provider. With no domain at all, a self-signed certificate is the honest
// answer: encrypted, unverifiable, and said so plainly in the panel rather
// than left for the browser to explain.
package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Modes.
const (
	// ModeOff serves plain HTTP.
	ModeOff = "off"
	// ModeACME obtains a certificate from a certificate authority.
	ModeACME = "acme"
	// ModeSelfSigned generates one locally.
	ModeSelfSigned = "self-signed"
)

// Challenge types.
const (
	// ChallengeHTTP01 answers on port 80. Needs nothing but reachability.
	ChallengeHTTP01 = "http-01"
	// ChallengeDNS01 answers with a TXT record. Works where port 80 does not,
	// and is the only way to get a wildcard.
	ChallengeDNS01 = "dns-01"
)

// MaxRenewBefore caps how early a certificate is replaced.
//
// Thirty days is the familiar figure, and for the ninety-day certificates most
// authorities issue it is exactly a third of the lifetime.
const MaxRenewBefore = 30 * 24 * time.Hour

// MinRenewBefore floors it, so a very short-lived certificate still leaves
// room for a few failed attempts.
const MinRenewBefore = 12 * time.Hour

// renewBefore is how long before expiry a certificate is replaced.
//
// A third of its lifetime rather than a fixed month. Authorities have started
// issuing certificates that last days rather than months — Let's Encrypt's
// short-lived profile is six — and a fixed thirty-day threshold would mean
// every such certificate is already "due for renewal" the moment it is issued.
// A panel that restarts twice would then order twice, and the third order
// would meet the rate limit with nothing to show for it.
//
// Discovered by issuing against a real test authority, whose certificates last
// under a week: the fixed threshold made every restart order a new one.
func renewBefore(notBefore, notAfter time.Time) time.Duration {
	lifetime := notAfter.Sub(notBefore)
	if lifetime <= 0 {
		return MinRenewBefore
	}

	window := lifetime / 3
	if window > MaxRenewBefore {
		window = MaxRenewBefore
	}
	if window < MinRenewBefore {
		window = MinRenewBefore
	}
	return window
}

// Errors callers distinguish.
var (
	// ErrNoCertificate reports that nothing has been obtained yet.
	ErrNoCertificate = errors.New("certs: no certificate available")
	// ErrNotConfigured reports a mode that cannot work with what it was given.
	ErrNotConfigured = errors.New("certs: not configured")
)

// Config describes how the certificate is obtained.
type Config struct {
	// Mode is off, acme or self-signed.
	Mode string
	// Domain is the name the certificate covers. Required for acme.
	Domain string
	// Email is the ACME account contact. Optional, but a certificate
	// authority uses it to warn about expiry, which is worth having.
	Email string
	// Challenge is http-01 or dns-01. Empty means http-01.
	Challenge string
	// DirectoryURL overrides the certificate authority. Empty means Let's
	// Encrypt. Pointed at a local test authority by the integration tests.
	DirectoryURL string
	// Dir is where certificates and the account key are stored.
	Dir string
	// AcceptTOS must be true to use a public authority: their terms require
	// agreement, and agreeing on an operator's behalf without telling them
	// would be putting words in their mouth. The installer asks.
	AcceptTOS bool

	// HTTPClient talks to the certificate authority. Nil uses the default.
	//
	// Exists so the integration tests can point at a local authority whose
	// certificate nothing trusts — without it, the ACME path could only ever
	// be checked against the real Let's Encrypt, which needs a public domain.
	HTTPClient *http.Client
	// Resolver checks whether a DNS-01 record has become visible. Nil uses
	// the system resolver.
	Resolver *net.Resolver
}

// Status is what the panel shows about the certificate.
type Status struct {
	// Mode is what was configured.
	Mode string `json:"mode"`
	// Domain the certificate covers.
	Domain string `json:"domain,omitempty"`
	// Trusted reports whether a browser will accept it without a warning.
	// False for self-signed, and the panel says why rather than leaving the
	// browser to.
	Trusted bool `json:"trusted"`
	// NotAfter is when the current certificate expires.
	NotAfter time.Time `json:"not_after,omitzero"`
	// Issuer names who signed it.
	Issuer string `json:"issuer,omitempty"`
	// Error is the last failure, if the certificate may be stale or missing.
	Error string `json:"error,omitempty"`
}

// DNSSolver answers a DNS-01 challenge.
//
// Narrow on purpose: the certificate manager has no business publishing
// address or SRV records, and the interface says so.
type DNSSolver interface {
	// EnsureTXT publishes the challenge tokens at sub.
	EnsureTXT(ctx context.Context, sub string, values []string) error
	// DeleteTXT removes them once the challenge is answered.
	DeleteTXT(ctx context.Context, sub string) error
	// Zone is the domain the solver writes into.
	Zone() string
}

// Manager holds the certificate and keeps it current.
type Manager struct {
	cfg    Config
	solver DNSSolver
	log    *slog.Logger

	// httpSolver serves the HTTP-01 challenge; nil for other modes.
	httpSolver *httpSolver

	mu      sync.RWMutex
	current *tls.Certificate
	lastErr error
}

// New builds a manager. It obtains nothing yet; call Start.
func New(cfg Config, solver DNSSolver, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}

	cfg.Mode = strings.TrimSpace(cfg.Mode)
	if cfg.Mode == "" {
		cfg.Mode = ModeOff
	}
	cfg.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Domain), "."))
	if cfg.Challenge == "" {
		cfg.Challenge = ChallengeHTTP01
	}

	switch cfg.Mode {
	case ModeOff:
	case ModeSelfSigned:
		if cfg.Dir == "" {
			return nil, fmt.Errorf("%w: self-signed certificates need a directory to live in", ErrNotConfigured)
		}
	case ModeACME:
		if cfg.Domain == "" {
			return nil, fmt.Errorf("%w: acme needs a domain", ErrNotConfigured)
		}
		if cfg.Dir == "" {
			return nil, fmt.Errorf("%w: acme needs a directory for its account key and certificates", ErrNotConfigured)
		}
		if !cfg.AcceptTOS {
			// Agreeing to someone else's terms on their behalf is putting
			// words in their mouth, so this is refused rather than defaulted.
			return nil, fmt.Errorf("%w: the certificate authority's terms must be accepted "+
				"(tls.accept_tos: true)", ErrNotConfigured)
		}
		switch cfg.Challenge {
		case ChallengeHTTP01:
		case ChallengeDNS01:
			if solver == nil {
				return nil, fmt.Errorf("%w: the dns-01 challenge needs a DNS provider, "+
					"but none is configured", ErrNotConfigured)
			}
		default:
			return nil, fmt.Errorf("%w: challenge must be %s or %s",
				ErrNotConfigured, ChallengeHTTP01, ChallengeDNS01)
		}
	default:
		return nil, fmt.Errorf("%w: mode must be %s, %s or %s",
			ErrNotConfigured, ModeOff, ModeACME, ModeSelfSigned)
	}

	m := &Manager{cfg: cfg, solver: solver, log: log}
	if cfg.Mode == ModeACME && cfg.Challenge == ChallengeHTTP01 {
		m.httpSolver = &httpSolver{}
	}
	return m, nil
}

// Enabled reports whether HTTPS is served at all.
func (m *Manager) Enabled() bool { return m.cfg.Mode != ModeOff }

// Mode reports the configured mode.
func (m *Manager) Mode() string { return m.cfg.Mode }

// TLSConfig returns a configuration serving the managed certificate.
//
// GetCertificate rather than a fixed certificate, so a renewal takes effect
// without restarting the daemon — which for a panel that also supervises
// running servers is not a small thing.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.current == nil {
				return nil, ErrNoCertificate
			}
			return m.current, nil
		},
	}
}

// Status reports what the panel should show.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := Status{
		Mode:    m.cfg.Mode,
		Domain:  m.cfg.Domain,
		Trusted: m.cfg.Mode == ModeACME,
	}
	if m.lastErr != nil {
		status.Error = m.lastErr.Error()
	}
	if m.current != nil && m.current.Leaf != nil {
		status.NotAfter = m.current.Leaf.NotAfter
		status.Issuer = m.current.Leaf.Issuer.CommonName
	}
	return status
}

// Start obtains a certificate and keeps it renewed until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) error {
	if m.cfg.Mode == ModeOff {
		return nil
	}

	if err := m.obtain(ctx); err != nil {
		return err
	}

	go m.renewLoop(ctx)
	return nil
}

// renewLoop replaces the certificate before it expires.
//
// Checked daily rather than scheduled for the exact moment: a daemon that has
// been asleep, or whose clock moved, should notice on the next check rather
// than having missed a one-shot timer.
func (m *Manager) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !m.needsRenewal() {
				continue
			}
			m.log.Info("renewing the certificate")
			if err := m.obtain(ctx); err != nil {
				// Warned rather than fatal: the certificate in hand is still
				// valid for weeks, and there is time for the next attempt.
				m.log.Warn("renewing the certificate failed",
					slog.String("error", err.Error()))
			}
		}
	}
}

func (m *Manager) needsRenewal() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.current == nil || m.current.Leaf == nil {
		return true
	}
	leaf := m.current.Leaf
	return time.Until(leaf.NotAfter) < renewBefore(leaf.NotBefore, leaf.NotAfter)
}

// obtain gets a certificate by whichever means the mode calls for.
func (m *Manager) obtain(ctx context.Context) error {
	var (
		cert *tls.Certificate
		err  error
	)

	switch m.cfg.Mode {
	case ModeSelfSigned:
		cert, err = m.selfSigned()
	case ModeACME:
		cert, err = m.fromACME(ctx)
	default:
		return nil
	}

	m.mu.Lock()
	m.lastErr = err
	if err == nil {
		m.current = cert
	}
	m.mu.Unlock()

	if err != nil {
		return err
	}

	m.log.Info("certificate ready",
		slog.String("mode", m.cfg.Mode),
		slog.String("domain", m.cfg.Domain),
		slog.Time("expires", cert.Leaf.NotAfter))
	return nil
}
