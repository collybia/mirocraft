package panelclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/bots/internal/panelclient"
)

// --- addresses ---

// An operator pastes the panel's address out of the browser, which is where
// the trailing slash and the /api/v1 come from.
func TestNewAcceptsTheAddressesOperatorsActuallyType(t *testing.T) {
	cases := []struct {
		given string
		want  string
	}{
		{"https://panel.example.com", "https://panel.example.com"},
		{"https://panel.example.com/", "https://panel.example.com"},
		{"https://panel.example.com/api/v1", "https://panel.example.com"},
		{"https://panel.example.com/api/v1/", "https://panel.example.com"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		// No scheme: https, because a token should not travel in the clear
		// without someone saying so.
		{"panel.example.com", "https://panel.example.com"},
	}

	for _, c := range cases {
		t.Run(c.given, func(t *testing.T) {
			client, err := panelclient.New(c.given, "token")
			if err != nil {
				t.Fatalf("New(%q): %v", c.given, err)
			}
			if got := client.BaseURL(); got != c.want {
				t.Errorf("BaseURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNewRefusesAddressesThatCannotWork(t *testing.T) {
	for _, given := range []string{"", "   ", "ftp://panel.example.com", "https://"} {
		if _, err := panelclient.New(given, "token"); err == nil {
			t.Errorf("New(%q) was accepted", given)
		}
	}
}

// --- against the real panel ---

func TestHealthNeedsNoToken(t *testing.T) {
	p := newPanel(t)

	client, err := panelclient.New(p.server.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("status = %q, want ok", health.Status)
	}
}

func TestLoginReturnsAWorkingToken(t *testing.T) {
	p := newPanel(t)

	client, err := panelclient.New(p.server.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	session, err := client.Login(context.Background(), testEmail, testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.Token == "" {
		t.Fatal("login returned no token")
	}
	if session.User.Email != testEmail {
		t.Errorf("user = %q, want %q", session.User.Email, testEmail)
	}

	// The token has to be usable, which is the only thing that makes login
	// worth anything.
	client.SetToken(session.Token)
	me, err := client.Me(context.Background())
	if err != nil {
		t.Fatalf("Me with the session token: %v", err)
	}
	if me.ID != p.userID {
		t.Errorf("me.id = %q, want %q", me.ID, p.userID)
	}
	if len(me.Scopes) == 0 {
		t.Error("the session token reports no scopes")
	}
}

func TestLoginWithTheWrongPasswordIsUnauthorized(t *testing.T) {
	p := newPanel(t)

	client, err := panelclient.New(p.server.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Login(context.Background(), testEmail, "not the password")
	if !errors.Is(err, panelclient.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestServerRoundTrip(t *testing.T) {
	p := newPanel(t)
	client := p.client()
	ctx := context.Background()

	servers, err := client.ListServers(ctx, panelclient.ListServersOptions{})
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != p.serverID {
		t.Fatalf("ListServers = %+v, want the seeded server", servers)
	}
	if servers[0].Name != "survival" || servers[0].Core != "paper" {
		t.Errorf("the fields did not survive the round trip: %+v", servers[0])
	}

	one, err := client.GetServer(ctx, p.serverID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if one.RAMMb != 1024 || one.Port != 25565 {
		t.Errorf("ram = %d, port = %d, want 1024 and 25565", one.RAMMb, one.Port)
	}

	name := "renamed"
	updated, err := client.UpdateServer(ctx, p.serverID, panelclient.UpdateServerRequest{Name: &name})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if updated.Name != name {
		t.Errorf("name = %q, want %q", updated.Name, name)
	}
	// A patch leaves everything it did not mention alone.
	if updated.RAMMb != 1024 {
		t.Errorf("ram = %d after renaming, want 1024", updated.RAMMb)
	}
}

func TestUnknownServerIsNotFound(t *testing.T) {
	p := newPanel(t)

	_, err := p.client().GetServer(context.Background(), "01NOSUCHSERVER")
	if !errors.Is(err, panelclient.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestABadTokenIsUnauthorized(t *testing.T) {
	p := newPanel(t)

	client, err := panelclient.New(p.server.URL, "mc_not_a_real_token")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.ListServers(context.Background(), panelclient.ListServersOptions{})
	if !errors.Is(err, panelclient.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

// A rejected create has to come back with the field that was wrong, or a bot
// can only say "no".
func TestValidationErrorsCarryTheDetails(t *testing.T) {
	p := newPanel(t)

	_, err := p.client().CreateServer(context.Background(), panelclient.CreateServerRequest{
		Name: "", Core: "paper", Version: "1.21.4", RAMMb: 1024, EULAAccepted: true,
	})
	if !errors.Is(err, panelclient.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}

	var apiErr *panelclient.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not a *panelclient.Error", err)
	}
	if apiErr.Message == "" {
		t.Error("the rejection carries no message")
	}
	if apiErr.Details["field"] != "name" {
		t.Errorf("details = %v, want the offending field", apiErr.Details)
	}
}

// --- names, which is how people drive a bot ---

func TestFindServer(t *testing.T) {
	p := newPanel(t)
	client := p.client()
	ctx := context.Background()

	for _, name := range []string{"survival", "SURVIVAL", p.serverID} {
		found, err := client.FindServer(ctx, name)
		if err != nil {
			t.Fatalf("FindServer(%q): %v", name, err)
		}
		if found.ID != p.serverID {
			t.Errorf("FindServer(%q) = %q, want %q", name, found.ID, p.serverID)
		}
	}

	if _, err := client.FindServer(ctx, "nothing like it"); !errors.Is(err, panelclient.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if _, err := client.FindServer(ctx, ""); !errors.Is(err, panelclient.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

// Acting on a guess when two servers match would start the wrong world.
func TestFindServerRefusesAnAmbiguousName(t *testing.T) {
	p := newPanel(t)
	client := p.client()
	ctx := context.Background()

	for _, name := range []string{"survival-one", "survival-two"} {
		if _, err := client.CreateServer(ctx, panelclient.CreateServerRequest{
			Name: name, Core: "paper", Version: "1.21.4", RAMMb: 1024, EULAAccepted: true,
		}); err != nil {
			t.Fatalf("CreateServer(%q): %v", name, err)
		}
	}

	_, err := client.FindServer(ctx, "survival-")
	if !errors.Is(err, panelclient.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "survival-one") || !strings.Contains(err.Error(), "survival-two") {
		t.Errorf("error %q does not name the candidates", err)
	}
}

// --- errors the panel does not produce, but a proxy in front of it does ---

func TestAProxyErrorIsStillReadable(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("<html><body>too many requests</body></html>"))
	}))
	defer proxy.Close()

	client, err := panelclient.New(proxy.URL, "token")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.ListServers(context.Background(), panelclient.ListServersOptions{})
	if !errors.Is(err, panelclient.ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}

	var apiErr *panelclient.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not a *panelclient.Error", err)
	}
	if apiErr.RetryAfter != 12*time.Second {
		t.Errorf("retry after = %v, want 12s", apiErr.RetryAfter)
	}
	// The HTML must not end up in the message a bot posts into a chat.
	if strings.Contains(apiErr.Error(), "<html>") {
		t.Errorf("the error carries the page body: %q", apiErr.Error())
	}
}
