package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Server timings.
const (
	// handshakeTimeout bounds how long a connection may stay silent before
	// saying who it is. Without it, opening sockets and never speaking is
	// enough to exhaust the listener.
	defaultHandshakeTimeout = 10 * time.Second
	// pingEvery keeps the control connection alive through NAT tables that
	// drop idle mappings, and is how the relay learns that a home machine has
	// gone away without closing anything.
	pingEvery = 30 * time.Second
	// pongTimeout is how long a silent agent is tolerated after a ping.
	pongTimeout = 15 * time.Second
	// dialTimeout is how long a player waits for the agent to call back
	// before the relay gives up on them.
	dialTimeout = 10 * time.Second
	// writeTimeout bounds a write to a peer that stopped reading.
	writeTimeout = 10 * time.Second
)

// Tunnel is what the relay knows about one home machine that may connect.
type Tunnel struct {
	// Name is for the operator's logs, not for authentication.
	Name string
	// TokenHash is the SHA-256 of the token, hex encoded. The token itself is
	// never stored: the relay only ever needs to recognise one, and a file of
	// live credentials on a public machine is a file worth stealing.
	TokenHash string
	// Port is the public port players connect to.
	Port int
}

// Config configures a relay server.
type Config struct {
	// ControlAddr is where agents connect, e.g. ":7000".
	ControlAddr string
	// Tunnels are the home machines allowed to connect, by token hash.
	Tunnels []Tunnel
	// TLS wraps the control and session connections. Optional but expected in
	// production: without it the token crosses the network in the clear.
	TLS TLSConfig
	// HandshakeTimeout bounds how long a new connection may stay silent before
	// saying who it is. Zero means the default.
	//
	// Configurable because of what it is for in the test suite rather than in
	// production: the bug this protocol learned the hard way only shows up
	// after the handshake window has passed, and a test that waits ten seconds
	// to see it is a test nobody runs. A package-level variable would have
	// done the same job and raced with the server reading it.
	HandshakeTimeout time.Duration
	Log              *slog.Logger
}

// TLSConfig is the certificate the relay serves. Empty means plain TCP.
type TLSConfig struct {
	CertFile string
	KeyFile  string
}

// HashToken renders a token the way the relay stores it.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a token for a new tunnel.
func NewToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("relay: generating a token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Server accepts agents and forwards players to them.
type Server struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	agents  map[string]*agent        // by token hash
	waiting map[string]chan net.Conn // by session id
}

// agent is one connected home machine.
type agent struct {
	tunnel  Tunnel
	control net.Conn

	// writeMu serialises writes to the control connection: the ping loop and
	// the player-accept path both send on it.
	writeMu sync.Mutex
	closed  chan struct{}
	once    sync.Once
}

func (a *agent) send(verb string, args ...string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	// A deadline per write, set fresh. The first version relied on the one
	// left by the handshake, which covered reads and writes both and expired
	// ten seconds after the agent connected — so the first player to arrive
	// after that got a write error, and the tunnel dropped instead of
	// forwarding. Tests never saw it because a test player arrives at once.
	_ = a.control.SetWriteDeadline(time.Now().Add(writeTimeout))
	return WriteMessage(a.control, verb, args...)
}

func (a *agent) close() {
	a.once.Do(func() {
		close(a.closed)
		_ = a.control.Close()
	})
}

// NewServer builds a relay from a configuration.
func NewServer(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	return &Server{
		cfg:     cfg,
		log:     cfg.Log,
		agents:  make(map[string]*agent),
		waiting: make(map[string]chan net.Conn),
	}
}

// Run serves until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	control, err := s.listen(s.cfg.ControlAddr)
	if err != nil {
		return err
	}
	defer func() { _ = control.Close() }()
	s.log.Info("relay control listening", slog.String("addr", s.cfg.ControlAddr))

	var wg sync.WaitGroup
	for _, tunnel := range s.cfg.Tunnels {
		public, err := net.Listen("tcp", ":"+strconv.Itoa(tunnel.Port))
		if err != nil {
			return fmt.Errorf("relay: listening for players of %s: %w", tunnel.Name, err)
		}
		s.log.Info("relay tunnel ready",
			slog.String("tunnel", tunnel.Name), slog.Int("port", tunnel.Port))

		wg.Add(1)
		go func(t Tunnel, ln net.Listener) {
			defer wg.Done()
			<-ctx.Done()
			_ = ln.Close()
		}(tunnel, public)

		wg.Add(1)
		go func(t Tunnel, ln net.Listener) {
			defer wg.Done()
			s.acceptPlayers(ctx, t, ln)
		}(tunnel, public)
	}

	go func() {
		<-ctx.Done()
		_ = control.Close()
	}()

	for {
		conn, err := control.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("relay: accepting: %w", err)
		}
		go s.handshake(ctx, conn)
	}
}

// listen opens the control listener, wrapped in TLS when configured.
//
// Optional rather than mandatory because a relay is also run on a private
// network, or behind something that already terminates TLS, and refusing to
// start in those cases would only teach people to patch it out. When it is not
// configured the log says so, in the words that matter: the token is readable
// by anything on the path.
func (s *Server) listen(addr string) (net.Listener, error) {
	if s.cfg.TLS.CertFile == "" || s.cfg.TLS.KeyFile == "" {
		s.log.Warn("relay is not using TLS; agent tokens cross the network in the clear")
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("relay: listening on %s: %w", addr, err)
		}
		return ln, nil
	}

	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("relay: loading the certificate: %w", err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, fmt.Errorf("relay: listening on %s: %w", addr, err)
	}
	return ln, nil
}

// handshake decides what a freshly accepted connection is.
func (s *Server) handshake(ctx context.Context, conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))

	reader := bufio.NewReaderSize(conn, MaxLineBytes)
	msg, err := ReadMessage(reader)
	if err != nil {
		_ = conn.Close()
		return
	}

	switch msg.Verb {
	case verbHello:
		s.serveAgent(ctx, conn, reader, msg.Arg(0))
	case verbSession:
		s.pairSession(conn, msg.Arg(0), msg.Arg(1))
	default:
		_ = WriteMessage(conn, verbError, "expected", verbHello)
		_ = conn.Close()
	}
}

// authorize finds the tunnel a token belongs to.
//
// The comparison is constant time and against a hash, so neither the token nor
// how far a guess got can be read off the timing.
func (s *Server) authorize(token string) (Tunnel, bool) {
	given := HashToken(token)
	for _, tunnel := range s.cfg.Tunnels {
		if subtle.ConstantTimeCompare([]byte(given), []byte(tunnel.TokenHash)) == 1 {
			return tunnel, true
		}
	}
	return Tunnel{}, false
}

func (s *Server) serveAgent(ctx context.Context, conn net.Conn, reader *bufio.Reader, token string) {
	tunnel, ok := s.authorize(token)
	if !ok {
		_ = WriteMessage(conn, verbError, "unknown token")
		_ = conn.Close()
		s.log.Warn("relay rejected an agent", slog.String("from", conn.RemoteAddr().String()))
		return
	}

	// The handshake deadline covered this connection's whole life; from here
	// each read and each write sets its own.
	_ = conn.SetDeadline(time.Time{})

	a := &agent{tunnel: tunnel, control: conn, closed: make(chan struct{})}

	s.mu.Lock()
	// One home machine per tunnel. A second one replaces the first rather than
	// being refused: the usual reason for a duplicate is a machine that
	// reconnected before the relay noticed the old connection was dead, and
	// refusing would leave the tunnel wedged until a timeout nobody is
	// watching.
	if existing, ok := s.agents[tunnel.TokenHash]; ok {
		existing.close()
	}
	s.agents[tunnel.TokenHash] = a
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.agents[tunnel.TokenHash] == a {
			delete(s.agents, tunnel.TokenHash)
		}
		s.mu.Unlock()
		a.close()
		s.log.Info("relay agent gone", slog.String("tunnel", tunnel.Name))
	}()

	if err := a.send(verbReady, strconv.Itoa(tunnel.Port)); err != nil {
		return
	}
	s.log.Info("relay agent connected",
		slog.String("tunnel", tunnel.Name), slog.String("from", conn.RemoteAddr().String()))

	go s.keepAlive(ctx, a)

	// From here the control connection carries only replies. A silent agent
	// is a dead agent, so the deadline is refreshed by what it says, not by
	// what the relay sends.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(pingEvery + pongTimeout))
		msg, err := ReadMessage(reader)
		if err != nil {
			return
		}
		switch msg.Verb {
		case verbPong:
		case verbPing:
			if err := a.send(verbPong); err != nil {
				return
			}
		default:
			// Unknown verbs are ignored rather than fatal: a newer agent
			// saying something this relay does not know is not a reason to
			// drop somebody's server.
		}
	}
}

func (s *Server) keepAlive(ctx context.Context, a *agent) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.closed:
			return
		case <-ticker.C:
			if err := a.send(verbPing); err != nil {
				a.close()
				return
			}
		}
	}
}

// acceptPlayers takes connections on a tunnel's public port.
func (s *Server) acceptPlayers(ctx context.Context, tunnel Tunnel, ln net.Listener) {
	for {
		player, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("relay accept failed",
				slog.String("tunnel", tunnel.Name), slog.String("error", err.Error()))
			return
		}
		go s.forward(tunnel, player)
	}
}

// forward pairs one player with a connection dialled back by the agent.
func (s *Server) forward(tunnel Tunnel, player net.Conn) {
	defer func() { _ = player.Close() }()

	s.mu.Lock()
	a := s.agents[tunnel.TokenHash]
	s.mu.Unlock()

	if a == nil {
		// Nothing is listening at the other end. Closing without a word is
		// right: the peer is a Minecraft client, which has no idea what this
		// protocol is, and it renders a clean "can't connect".
		return
	}

	session, err := newSessionID()
	if err != nil {
		return
	}

	incoming := make(chan net.Conn, 1)
	s.mu.Lock()
	s.waiting[session] = incoming
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.waiting, session)
		s.mu.Unlock()
	}()

	if err := a.send(verbDial, session); err != nil {
		a.close()
		return
	}

	select {
	case tunnelConn := <-incoming:
		defer func() { _ = tunnelConn.Close() }()
		splice(player, tunnelConn)
	case <-time.After(dialTimeout):
		s.log.Warn("relay agent did not call back",
			slog.String("tunnel", tunnel.Name), slog.String("session", session))
	}
}

// pairSession hands a connection the agent dialled back to the player waiting
// for it.
func (s *Server) pairSession(conn net.Conn, token, session string) {
	if _, ok := s.authorize(token); !ok {
		_ = conn.Close()
		return
	}

	s.mu.Lock()
	waiting, ok := s.waiting[session]
	if ok {
		delete(s.waiting, session)
	}
	s.mu.Unlock()

	if !ok {
		// The player gave up, or the session was invented. Either way there is
		// nobody to join it to.
		_ = conn.Close()
		return
	}

	// The deadline set for the handshake must go, or the copy below would stop
	// mid-game.
	_ = conn.SetDeadline(time.Time{})
	waiting <- conn
}

// splice copies in both directions until either side is done.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	half := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		// Half-close so the other direction can drain: a player who has sent
		// everything should not have the server's reply cut off.
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
			return
		}
		_ = dst.Close()
	}

	go half(a, b)
	go half(b, a)
	wg.Wait()
}

func newSessionID() (string, error) {
	raw := make([]byte, SessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("relay: generating a session id")
	}
	return hex.EncodeToString(raw), nil
}
