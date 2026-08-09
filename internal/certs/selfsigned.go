package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// SelfSignedValidity is how long a generated certificate lasts.
//
// A year, and renewed a month before that: long enough that an operator who
// pinned the fingerprint is not re-pinning constantly, short enough that a
// forgotten panel does not carry the same key forever.
const SelfSignedValidity = 365 * 24 * time.Hour

// selfSigned loads the generated certificate, making one if needed.
//
// Reused across restarts rather than regenerated: a browser that was told to
// trust this exception, or an operator who checked the fingerprint, should not
// have to do it again every time the daemon restarts.
func (m *Manager) selfSigned() (*tls.Certificate, error) {
	certPath := filepath.Join(m.cfg.Dir, "self-signed.crt")
	keyPath := filepath.Join(m.cfg.Dir, "self-signed.key")

	if cert, err := loadPair(certPath, keyPath); err == nil {
		if time.Until(cert.Leaf.NotAfter) > renewBefore(cert.Leaf.NotBefore, cert.Leaf.NotAfter) {
			return cert, nil
		}
		m.log.Info("the self-signed certificate is near expiry, generating a new one")
	}

	cert, err := generateSelfSigned(m.cfg.Domain)
	if err != nil {
		return nil, err
	}
	if err := savePair(m.cfg.Dir, certPath, keyPath, cert); err != nil {
		// Worth reporting but not fatal: an unsaved certificate still serves,
		// it just will not survive a restart.
		m.log.Warn("saving the self-signed certificate failed",
			slog.String("error", err.Error()))
	}
	return cert, nil
}

// generateSelfSigned builds a certificate for a domain, an address, or
// neither.
func generateSelfSigned(domain string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generating a key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certs: generating a serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonNameFor(domain),
			Organization: []string{"Mirocraft (self-signed)"},
		},
		// A minute in the past: a client whose clock is slightly behind would
		// otherwise reject a certificate issued seconds ago, which is a
		// baffling failure to debug.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(SelfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	if domain != "" {
		template.DNSNames = []string{domain}
	} else {
		// No domain means the panel is reached by address, so the certificate
		// has to cover addresses — a name-only certificate would be rejected
		// for exactly the way it is going to be used.
		template.DNSNames = []string{"localhost"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		if locals, err := localAddresses(); err == nil {
			template.IPAddresses = append(template.IPAddresses, locals...)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("certs: creating the certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("certs: parsing the certificate just created: %w", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

func commonNameFor(domain string) string {
	if domain != "" {
		return domain
	}
	return "mirocraft"
}

// localAddresses returns the host's own addresses, so a certificate with no
// domain still matches the address an operator actually types.
func localAddresses() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}

	var out []net.IP
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipNet.IP)
	}
	return out, nil
}

// loadPair reads a stored certificate and its key.
func loadPair(certPath, keyPath string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	// LoadX509KeyPair leaves Leaf nil, and everything here — expiry, issuer,
	// renewal — reads it.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &cert, nil
}

// savePair writes a certificate and its key.
func savePair(dir, certPath, keyPath string, cert *tls.Certificate) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("certs: creating %s: %w", dir, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("certs: writing the certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return fmt.Errorf("certs: encoding the key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	// 0600: a private key readable by anyone on the host is not a private key.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("certs: writing the key: %w", err)
	}
	return nil
}
