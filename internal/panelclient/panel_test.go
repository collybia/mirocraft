package panelclient_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/api"
	"github.com/collybia/mirocraft/internal/mcping"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

// The client is tested against the real API rather than against a fake panel.
//
// A hand-written stub would only prove that the client parses the JSON the
// test author wrote, which is exactly the JSON they had in mind when writing
// the client. What has to hold is that the client and the panel agree, and
// the only way to check that is to run the panel.

const (
	testEmail     = "owner@example.com"
	testPassword  = "correct horse battery staple"
	fakeServerEnv = "MIROCRAFT_FAKE_PANEL_SERVER"
)

// TestMain doubles as the fake Minecraft server: the process re-executes
// itself, which keeps the suite portable and needs no Java.
func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) != "" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeServer() {
	_, _ = io.WriteString(os.Stdout, "[INFO] fake server ready\n")

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				command := strings.TrimRight(string(buf[:i]), "\r")
				buf = buf[i+1:]
				if command == "stop" {
					os.Exit(0)
				}
				_, _ = io.WriteString(os.Stdout, "[INFO] echo: "+command+"\n")
			}
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

// panel is a running API with one account and one server record.
type panel struct {
	t       *testing.T
	server  *httptest.Server
	db      *store.Store
	runner  *runner.ProcessRunner
	dataDir string

	userID   string
	serverID string
	token    string
}

func newPanel(t *testing.T) *panel {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	pr := runner.NewProcessRunner(silent())
	pr.Build = func(*runner.Server) (string, []string, error) { return self, nil, nil }
	pr.Env = append(os.Environ(), fakeServerEnv+"=1")

	dataDir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dataDir, "panel.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	a := api.New(api.Options{
		Store:     db,
		Console:   pr,
		Lifecycle: pr,
		DataDir:   dataDir,
		Logger:    silent(),
		// httptest sends no Origin, and the client sends the panel's own, so
		// the default same-origin check is left in place rather than opened.
		CheckOrigin: func(r *http.Request) bool {
			return r.Header.Get("Origin") == "" || strings.HasPrefix(r.Header.Get("Origin"), "http://127.0.0.1")
		},
		// Nothing here speaks the Minecraft protocol.
		Ping: func(context.Context, string, int) (*mcping.Status, error) {
			return nil, io.EOF
		},
	})

	srv := httptest.NewServer(a.Handler())

	p := &panel{t: t, server: srv, db: db, runner: pr, dataDir: dataDir}
	t.Cleanup(func() {
		srv.Close()
		_ = pr.Shutdown(context.Background())
		_ = db.Close()
	})
	p.seed()
	return p
}

func (p *panel) seed() {
	p.t.Helper()
	ctx := context.Background()

	hash, err := store.HashPassword(testPassword)
	if err != nil {
		p.t.Fatalf("hashing the password: %v", err)
	}
	user := &store.User{Email: testEmail, PasswordHash: hash, Role: store.RoleAdmin}
	if err := p.db.Users.Create(ctx, user); err != nil {
		p.t.Fatalf("creating the user: %v", err)
	}
	p.userID = user.ID

	value, tokenHash, err := store.GenerateToken()
	if err != nil {
		p.t.Fatalf("generating the token: %v", err)
	}
	if err := p.db.Tokens.Create(ctx, &store.Token{
		UserID: user.ID, Name: "bot", Hash: tokenHash,
		Scopes: api.AllScopes, Kind: store.TokenKindAPI,
	}); err != nil {
		p.t.Fatalf("creating the token: %v", err)
	}
	p.token = value

	p.serverID = "01TESTPANELSERVER"
	record := &store.Server{
		ID: p.serverID, OwnerID: user.ID, Name: "survival", Core: "paper",
		Version: "1.21.4", RAMMb: 1024, Port: 25565,
		Dir: "servers/" + p.serverID,
	}
	if err := p.db.Servers.Create(ctx, record); err != nil {
		p.t.Fatalf("creating the server record: %v", err)
	}
	// The record is not enough: starting a server changes into its directory,
	// and one that is not there fails with a message about a path rather than
	// about a missing server.
	if err := os.MkdirAll(filepath.Join(p.dataDir, "servers", p.serverID), 0o750); err != nil {
		p.t.Fatalf("creating the server directory: %v", err)
	}
}

// client returns a client authenticated with the seeded API token.
func (p *panel) client() *panelclient.Client {
	p.t.Helper()
	c, err := panelclient.New(p.server.URL, p.token)
	if err != nil {
		p.t.Fatalf("building the client: %v", err)
	}
	return c
}

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
