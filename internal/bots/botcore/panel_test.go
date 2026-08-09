package botcore_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/api"
	"github.com/collybia/mirocraft/internal/bots/botcore"
	"github.com/collybia/mirocraft/internal/mcping"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

const (
	testEmail     = "owner@example.com"
	fakeServerEnv = "MIROCRAFT_FAKE_BOTCORE_SERVER"
)

// The test binary stands in for a Minecraft server, so the suite needs no Java.
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

// env is a running panel, a bot client and the commands built on them.
type env struct {
	db       *store.Store
	cmd      *botcore.Commands
	panelURL string
	userID   string
	dataDir  string
	owner    *panelclient.Client
}

func newEnv(t *testing.T) *env {
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
		Store: db, Console: pr, Lifecycle: pr, DataDir: dataDir, Logger: silent(),
		Ping: func(context.Context, string, int) (*mcping.Status, error) { return nil, io.EOF },
	})
	srv := httptest.NewServer(a.Handler())

	e := &env{db: db, panelURL: srv.URL, dataDir: dataDir}
	t.Cleanup(func() {
		srv.Close()
		_ = pr.Shutdown(context.Background())
		_ = db.Close()
	})

	e.seed(t)
	return e
}

func (e *env) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	hash, err := store.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	owner := &store.User{Email: testEmail, PasswordHash: hash, Role: store.RoleUser}
	if err := e.db.Users.Create(ctx, owner); err != nil {
		t.Fatalf("creating the owner: %v", err)
	}
	e.userID = owner.ID

	// The owner's own token, for the parts of a test that act as the person
	// rather than as the bot.
	e.owner = e.client(t, owner.ID, api.AllScopes)

	// The bot: its own account, and a token that may act for linked people
	// and nothing else.
	botUser := &store.User{Email: "bot@example.com", PasswordHash: hash, Role: store.RoleUser}
	if err := e.db.Users.Create(ctx, botUser); err != nil {
		t.Fatalf("creating the bot account: %v", err)
	}
	botClient := e.client(t, botUser.ID, []string{api.ScopeIntegrationsAct})

	e.cmd = &botcore.Commands{
		Client:   botClient,
		Provider: store.ProviderDiscord,
		PanelURL: e.panelURL,
	}

	server := &store.Server{
		ID: "01TESTBOTSERVER", OwnerID: owner.ID, Name: "survival", Core: "paper",
		Version: "1.21.4", RAMMb: 1024, Port: 25565, Dir: "servers/01TESTBOTSERVER",
	}
	if err := e.db.Servers.Create(ctx, server); err != nil {
		t.Fatalf("creating the server: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(e.dataDir, "servers", server.ID), 0o750); err != nil {
		t.Fatalf("creating the server directory: %v", err)
	}
}

// client mints a token with the given scopes and wraps it in a panel client.
func (e *env) client(t *testing.T, userID string, scopes []string) *panelclient.Client {
	t.Helper()

	value, hash, err := store.GenerateToken()
	if err != nil {
		t.Fatalf("generating a token: %v", err)
	}
	if err := e.db.Tokens.Create(context.Background(), &store.Token{
		UserID: userID, Name: "test", Hash: hash, Scopes: scopes, Kind: store.TokenKindAPI,
	}); err != nil {
		t.Fatalf("creating a token: %v", err)
	}

	client, err := panelclient.New(e.panelURL, value)
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	return client
}

// issueCode asks the panel for a linking code, the way the web panel does.
func (e *env) issueCode(t *testing.T) string {
	t.Helper()

	code, _, err := e.db.Integrations.IssueCode(context.Background(), store.ProviderDiscord, e.userID)
	if err != nil {
		t.Fatalf("issuing a code: %v", err)
	}
	return code
}

// link connects a chat account to the seeded owner.
func (e *env) link(t *testing.T, externalID string) {
	t.Helper()

	code := e.issueCode(t)
	if _, err := e.db.Integrations.Redeem(context.Background(), store.ProviderDiscord, code, externalID); err != nil {
		t.Fatalf("linking: %v", err)
	}
}

// createServer adds another server to the owner, for the tests about names.
func (e *env) createServer(t *testing.T, name string) {
	t.Helper()

	if _, err := e.owner.CreateServer(context.Background(), panelclient.CreateServerRequest{
		Name: name, Core: "paper", Version: "1.21.4", RAMMb: 1024, EULAAccepted: true,
	}); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
