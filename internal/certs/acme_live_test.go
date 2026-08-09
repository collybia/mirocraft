package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// These issue a real certificate from a real ACME server.
//
// Pebble is Let's Encrypt's own test authority: it speaks the same protocol,
// makes the same demands and rejects the same mistakes, but runs locally and
// needs no public domain. Without it this package could only be checked
// against Let's Encrypt itself, which no test should touch — so the ACME path,
// the part most likely to be subtly wrong, would be verified by nothing but
// the compiler.
//
// Skipped unless MIROCRAFT_ACME points at a running Pebble. To start one:
//
//	docker network create mirocraft-acme
//	docker run -d --name challtestsrv --network mirocraft-acme -p 8055:8055 \
//	  ghcr.io/letsencrypt/pebble-challtestsrv -defaultIPv4 "" -defaultIPv6 ""
//	docker run -d --name pebble --network mirocraft-acme -p 14000:14000 -p 15000:15000 \
//	  -e PEBBLE_VA_NOSLEEP=1 ghcr.io/letsencrypt/pebble \
//	  -config /test/config/pebble-config.json -dnsserver challtestsrv:8053
//	docker cp pebble:/test/certs/pebble.minica.pem ./pebble-ca.pem
//	MIROCRAFT_ACME=https://127.0.0.1:14000/dir //	MIROCRAFT_ACME_CA=./pebble-ca.pem go test ./internal/certs/ -run Live
//
// Two certificate authorities are involved and they are not the same one:
// Pebble serves its own API over a certificate from pebble.minica.pem, and
// issues certificates from a different root at /roots/0. Trusting the wrong
// one gives "certificate signed by unknown authority" on the very first
// request, which is a confusing way to learn this.
const (
	challtestsrvURL      = "http://127.0.0.1:8055"
	challtestsrvDNSAddr  = "127.0.0.1:8053"
	pebbleAccountContact = "acme@example.test"
)

func requirePebble(t *testing.T) (directory string, client *http.Client) {
	t.Helper()

	directory = os.Getenv("MIROCRAFT_ACME")
	if directory == "" {
		t.Skip("set MIROCRAFT_ACME to a Pebble directory URL to run the ACME tests")
	}

	// Pebble serves its API over a certificate nothing trusts, so its
	// authority has to be supplied. Trusting it is exactly what an operator
	// would not do for a real authority — and exactly the point of a test one.
	roots, err := pebbleAPIRoots()
	if err != nil {
		t.Skipf("cannot read Pebble's API certificate authority: %v", err)
	}

	client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
	}
	return directory, client
}

// pebbleAPIRoots reads the authority that signed Pebble's own API
// certificate, which is not the one it issues from.
func pebbleAPIRoots() (*x509.CertPool, error) {
	path := os.Getenv("MIROCRAFT_ACME_CA")
	if path == "" {
		return nil, fmt.Errorf("set MIROCRAFT_ACME_CA to Pebble's pebble.minica.pem")
	}

	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("%s is not a certificate", path)
	}
	return pool, nil
}

// challtestsrvSolver publishes TXT records into Pebble's DNS server.
//
// A stand-in for a real provider, and a fair one: the interface it implements
// is the same three methods deSEC, DuckDNS and Cloudflare implement, and those
// are checked separately against their own APIs' documented shapes.
type challtestsrvSolver struct {
	zone string
	t    *testing.T
}

func (s challtestsrvSolver) Zone() string { return s.zone }

func (s challtestsrvSolver) EnsureTXT(ctx context.Context, sub string, values []string) error {
	host := strings.TrimSuffix(sub+"."+s.zone, ".") + "."
	for _, value := range values {
		if err := s.post(ctx, "/set-txt", map[string]string{"host": host, "value": value}); err != nil {
			return err
		}
	}
	return nil
}

func (s challtestsrvSolver) DeleteTXT(ctx context.Context, sub string) error {
	host := strings.TrimSuffix(sub+"."+s.zone, ".") + "."
	return s.post(ctx, "/clear-txt", map[string]string{"host": host})
}

func (s challtestsrvSolver) post(ctx context.Context, path string, body map[string]string) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, challtestsrvURL+path,
		bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("challtestsrv %s: %s", path, resp.Status)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// challtestsrvResolver queries Pebble's DNS server rather than the host's, so
// the propagation wait sees the same records the authority will.
func challtestsrvResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", challtestsrvDNSAddr)
		},
	}
}

// The whole DNS-01 path against a real authority: order, challenge, publish,
// accept, poll, CSR, certificate.
func TestLiveIssuesACertificateOverDNS01(t *testing.T) {
	directory, client := requirePebble(t)

	const domain = "panel.mirocraft.test"
	dir := t.TempDir()

	m, err := New(Config{
		Mode: ModeACME, Domain: domain, Email: pebbleAccountContact,
		Challenge: ChallengeDNS01, DirectoryURL: directory, Dir: dir,
		AcceptTOS: true, HTTPClient: client, Resolver: challtestsrvResolver(),
	}, challtestsrvSolver{zone: domain, t: t}, silent())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("obtaining a certificate: %v", err)
	}

	status := m.Status()
	if !status.Trusted {
		t.Error("an ACME certificate is not reported as trusted")
	}
	if status.NotAfter.Before(time.Now()) {
		t.Errorf("the certificate is already expired: %s", status.NotAfter)
	}
	t.Logf("issued by %q until %s", status.Issuer, status.NotAfter.Format(time.RFC3339))

	cert, err := m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	// It has to cover the name it was ordered for — a certificate for
	// something else is worse than none, because the browser explains it and
	// the panel does not.
	if err := cert.Leaf.VerifyHostname(domain); err != nil {
		t.Fatalf("the issued certificate does not cover %s: %v", domain, err)
	}
	// And it must chain to the authority, not be self-signed by accident.
	if cert.Leaf.Issuer.CommonName == cert.Leaf.Subject.CommonName {
		t.Error("the certificate is self-issued, so nothing was obtained from the authority")
	}

	// Both the certificate and the account key on disk: without the account
	// key a restart registers a new account and starts the rate limit over.
	for _, name := range []string{"acme.crt", "acme.key", "acme-account.key"} {
		if _, err := os.Stat(dir + string(os.PathSeparator) + name); err != nil {
			t.Errorf("%s was not saved: %v", name, err)
		}
	}
}

// A restart must not ask for a new certificate: that is how a panel that
// restarts often runs into the authority's rate limit and ends up with none.
func TestLiveReusesTheStoredCertificate(t *testing.T) {
	directory, client := requirePebble(t)

	const domain = "reuse.mirocraft.test"
	dir := t.TempDir()

	build := func() *Manager {
		m, err := New(Config{
			Mode: ModeACME, Domain: domain, Challenge: ChallengeDNS01,
			DirectoryURL: directory, Dir: dir, AcceptTOS: true,
			HTTPClient: client, Resolver: challtestsrvResolver(),
		}, challtestsrvSolver{zone: domain, t: t}, silent())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return m
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first := build()
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first issuance: %v", err)
	}
	firstExpiry := first.Status().NotAfter

	second := build()
	if err := second.Start(ctx); err != nil {
		t.Fatalf("second start: %v", err)
	}

	if !second.Status().NotAfter.Equal(firstExpiry) {
		t.Fatal("a restart obtained a new certificate instead of reusing the stored one")
	}
}

// The token must not be left behind: anyone who can read the zone can see it,
// and it has no use after the challenge.
func TestLiveRemovesTheChallengeRecord(t *testing.T) {
	directory, client := requirePebble(t)

	const domain = "cleanup.mirocraft.test"

	m, err := New(Config{
		Mode: ModeACME, Domain: domain, Challenge: ChallengeDNS01,
		DirectoryURL: directory, Dir: t.TempDir(), AcceptTOS: true,
		HTTPClient: client, Resolver: challtestsrvResolver(),
	}, challtestsrvSolver{zone: domain, t: t}, silent())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("obtaining a certificate: %v", err)
	}

	values, err := challtestsrvResolver().LookupTXT(ctx, "_acme-challenge."+domain)
	if err == nil && len(values) > 0 {
		t.Fatalf("the challenge record is still published: %v", values)
	}
}
