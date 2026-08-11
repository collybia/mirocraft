package relay

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

// AgentConfig is what the daemon needs to reach a relay.
type AgentConfig struct {
	// Addr is the relay's control address, host:port.
	Addr string
	// Token authenticates this tunnel. Never logged.
	Token string
	// Target is where the traffic goes on this machine, e.g. "127.0.0.1:25565".
	Target string
	// Insecure skips certificate verification. For a relay somebody runs for
	// themselves with a self-signed certificate; the flag is named for what it
	// costs rather than for what it enables.
	Insecure bool
	// Fingerprint pins the relay's certificate by SHA-256, hex encoded. The
	// honest middle between a public certificate and no verification at all:
	// a self-hosted relay can be verified exactly, without a certificate
	// authority.
	Fingerprint string
	Log         *slog.Logger
}

// Agent keeps one tunnel to a relay open.
type Agent struct {
	cfg AgentConfig
	log *slog.Logger

	// port is the public port the relay assigned, readable once connected.
	port   chan int
	closed chan struct{}
}

// NewAgent builds an agent. Nothing is dialled until Run.
func NewAgent(cfg AgentConfig) *Agent {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Agent{cfg: cfg, log: cfg.Log, port: make(chan int, 1), closed: make(chan struct{})}
}

// Run keeps the tunnel up until the context is cancelled.
//
// Reconnects on its own, with a backoff: a home machine's connection drops,
// and a tunnel that gave up the first time it did would be a tunnel that is
// down every morning.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second

	for {
		err := a.session(ctx)
		// Being told to stop is not a failure, whatever the connection was
		// doing when it was cut.
		if ctx.Err() != nil {
			return nil //nolint:nilerr // cancellation, not the session's error
		}
		if err != nil {
			a.log.Warn("relay tunnel dropped",
				slog.String("relay", a.cfg.Addr), slog.String("error", err.Error()))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Port reports the public port, waiting for the first connection to say what
// it is. Returns zero if the context ends first.
func (a *Agent) Port(ctx context.Context) int {
	select {
	case port := <-a.port:
		// Put it back: the port outlives one read, and callers ask repeatedly.
		select {
		case a.port <- port:
		default:
		}
		return port
	case <-ctx.Done():
		return 0
	}
}

// session runs one control connection from HELLO until it fails.
func (a *Agent) session(ctx context.Context) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := WriteMessage(conn, verbHello, a.cfg.Token); err != nil {
		return err
	}

	reader := bufio.NewReaderSize(conn, MaxLineBytes)
	_ = conn.SetReadDeadline(time.Now().Add(defaultHandshakeTimeout))
	msg, err := ReadMessage(reader)
	if err != nil {
		return err
	}
	if msg.Verb == verbError {
		// Terminal on purpose: a wrong token will still be wrong in a second,
		// and retrying it forever is how a typo becomes a login attempt every
		// two seconds for a week.
		return fmt.Errorf("relay refused the tunnel: %s", strings.Join(msg.Args, " "))
	}
	if msg.Verb != verbReady {
		return ErrMalformed
	}

	port, err := ParsePort(msg.Arg(0))
	if err != nil {
		return err
	}
	select {
	case a.port <- port:
	default:
	}
	a.log.Info("relay tunnel up",
		slog.String("relay", a.cfg.Addr), slog.Int("public_port", port))

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(pingEvery + pongTimeout))
		msg, err := ReadMessage(reader)
		if err != nil {
			return err
		}

		switch msg.Verb {
		case verbPing:
			if err := WriteMessage(conn, verbPong); err != nil {
				return err
			}
		case verbDial:
			session := msg.Arg(0)
			if !validSessionID(session) {
				continue
			}
			// Each player gets its own goroutine and its own connection; the
			// control link must stay free to accept the next one.
			go a.serve(ctx, session)
		case verbError:
			return fmt.Errorf("relay: %s", strings.Join(msg.Args, " "))
		}
	}
}

// serve dials the relay back for one player and joins it to the local server.
func (a *Agent) serve(ctx context.Context, session string) {
	local, err := net.DialTimeout("tcp", a.cfg.Target, dialTimeout)
	if err != nil {
		// The server is not running. Nothing is sent back: the relay will time
		// the session out, and the player's client shows the same "cannot
		// connect" it would get from a real server that is down.
		a.log.Debug("relay could not reach the local server",
			slog.String("target", a.cfg.Target), slog.String("error", err.Error()))
		return
	}
	defer func() { _ = local.Close() }()

	remote, err := a.dial(ctx)
	if err != nil {
		return
	}
	defer func() { _ = remote.Close() }()

	if err := WriteMessage(remote, verbSession, a.cfg.Token, session); err != nil {
		return
	}
	_ = remote.SetDeadline(time.Time{})

	splice(local, remote)
}

// dial opens a connection to the relay, with TLS unless it was turned off.
func (a *Agent) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: defaultHandshakeTimeout}

	if a.cfg.Insecure && a.cfg.Fingerprint == "" {
		conn, err := dialer.DialContext(ctx, "tcp", a.cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("relay: dialling %s: %w", a.cfg.Addr, err)
		}
		return conn, nil
	}

	host, _, err := net.SplitHostPort(a.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("relay: %q is not host:port: %w", a.cfg.Addr, err)
	}

	config := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if a.cfg.Fingerprint != "" {
		// Pinned: the certificate does not have to be signed by anybody, but
		// it has to be exactly the one the operator wrote down.
		config.InsecureSkipVerify = true // #nosec G402 -- verified by fingerprint below
		// VerifyConnection rather than VerifyPeerCertificate: the latter is
		// not called when a TLS session is resumed, so a pinned connection
		// could be re-established later without the pin ever being checked.
		config.VerifyConnection = pinned(a.cfg.Fingerprint)
	}

	conn, err := tls.DialWithDialer(dialer, "tcp", a.cfg.Addr, config)
	if err != nil {
		return nil, fmt.Errorf("relay: dialling %s: %w", a.cfg.Addr, err)
	}
	return conn, nil
}

// pinned builds a verifier that accepts exactly one certificate.
func pinned(fingerprint string) func(tls.ConnectionState) error {
	want := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))

	return func(state tls.ConnectionState) error {
		for _, cert := range state.PeerCertificates {
			if Fingerprint(cert.Raw) == want {
				return nil
			}
		}
		return errors.New("relay: the certificate does not match the pinned fingerprint")
	}
}

// Fingerprint renders a certificate's SHA-256 the way the configuration writes
// it: lowercase hex, no separators.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func validSessionID(id string) bool {
	if len(id) != SessionIDBytes*2 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
