package panelclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/collybia/mirocraft/internal/api"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/store"
)

// botClient returns a client holding a bot token, the way a bot is configured.
func (p *panel) botClient(t *testing.T) *panelclient.Client {
	t.Helper()

	ctx := context.Background()
	botUser := &store.User{Email: "bot@example.com", PasswordHash: "x", Role: store.RoleUser}
	if err := p.db.Users.Create(ctx, botUser); err != nil {
		t.Fatalf("creating the bot account: %v", err)
	}

	value, hash, err := store.GenerateToken()
	if err != nil {
		t.Fatalf("generating the bot token: %v", err)
	}
	if err := p.db.Tokens.Create(ctx, &store.Token{
		UserID: botUser.ID, Name: "discord", Hash: hash,
		Scopes: []string{api.ScopeIntegrationsAct}, Kind: store.TokenKindAPI,
	}); err != nil {
		t.Fatalf("creating the bot token: %v", err)
	}

	client, err := panelclient.New(p.server.URL, value)
	if err != nil {
		t.Fatalf("building the bot client: %v", err)
	}
	return client
}

// The flow end to end: a person asks the panel for a code, types it to the
// bot, and from then on the bot's commands answer with that person's servers.
func TestLinkThenActOnBehalf(t *testing.T) {
	p := newPanel(t)
	ctx := context.Background()
	bot := p.botClient(t)

	// The code is issued to the person by the panel, through their own client.
	code, _, err := p.db.Integrations.IssueCode(ctx, store.ProviderDiscord, p.userID)
	if err != nil {
		t.Fatalf("issuing a code: %v", err)
	}

	linked, err := bot.Link(ctx, panelclient.ProviderDiscord, code, "31337")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if linked.UserID != p.userID {
		t.Fatalf("linked user = %q, want %q", linked.UserID, p.userID)
	}

	// Acting as itself, the bot sees nothing: its own account owns no servers
	// and its token cannot even list them.
	if _, err := bot.ListServers(ctx, panelclient.ListServersOptions{}); err == nil {
		t.Fatal("the bot listed servers as itself")
	}

	// Acting for the person, it sees theirs.
	forPerson := bot.For(panelclient.ProviderDiscord, "31337")
	servers, err := forPerson.ListServers(ctx, panelclient.ListServersOptions{})
	if err != nil {
		t.Fatalf("ListServers on behalf: %v", err)
	}
	if len(servers) != 1 || servers[0].ID != p.serverID {
		t.Fatalf("servers = %+v, want the linked person's", servers)
	}

	// And it can do what a bot is for.
	if _, err := forPerson.Start(ctx, p.serverID); err != nil {
		t.Fatalf("Start on behalf: %v", err)
	}
}

// The cap is the point of the design, so it is asserted from the client side
// too: what the panel refuses must surface as a refusal, not as a surprise.
func TestADelegatedClientCannotWriteFiles(t *testing.T) {
	p := newPanel(t)
	ctx := context.Background()
	bot := p.botClient(t)

	code, _, err := p.db.Integrations.IssueCode(ctx, store.ProviderDiscord, p.userID)
	if err != nil {
		t.Fatalf("issuing a code: %v", err)
	}
	if _, err := bot.Link(ctx, panelclient.ProviderDiscord, code, "31337"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	forPerson := bot.For(panelclient.ProviderDiscord, "31337")

	// Deleting a server needs servers:write, which is outside the delegatable
	// set however the person is privileged.
	if err := forPerson.DeleteServer(ctx, p.serverID); !errors.Is(err, panelclient.ErrForbidden) {
		t.Fatalf("DeleteServer: %v, want ErrForbidden", err)
	}
}

func TestLinkingWithABadCodeSaysSo(t *testing.T) {
	p := newPanel(t)
	bot := p.botClient(t)

	_, err := bot.Link(context.Background(), panelclient.ProviderDiscord, "ZZZZ-ZZZZ", "31337")
	if !errors.Is(err, panelclient.ErrLinkCodeInvalid) {
		t.Fatalf("Link: %v, want ErrLinkCodeInvalid", err)
	}
}

// For must not mutate the client it was called on, or one command's identity
// would leak into the next.
func TestForLeavesTheOriginalAlone(t *testing.T) {
	p := newPanel(t)
	bot := p.botClient(t)

	first := bot.For(panelclient.ProviderDiscord, "1")
	second := bot.For(panelclient.ProviderDiscord, "2")

	if bot.OnBehalfOf() != "" {
		t.Errorf("the original client now acts for %q", bot.OnBehalfOf())
	}
	if first.OnBehalfOf() != "discord:1" || second.OnBehalfOf() != "discord:2" {
		t.Errorf("clients act for %q and %q", first.OnBehalfOf(), second.OnBehalfOf())
	}
}

// Unlinking has to work from the chat, so it goes through the delegated
// client rather than the panel.
func TestUnlinkFromTheChat(t *testing.T) {
	p := newPanel(t)
	ctx := context.Background()
	bot := p.botClient(t)

	code, _, err := p.db.Integrations.IssueCode(ctx, store.ProviderDiscord, p.userID)
	if err != nil {
		t.Fatalf("issuing a code: %v", err)
	}
	if _, err := bot.Link(ctx, panelclient.ProviderDiscord, code, "31337"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	forPerson := bot.For(panelclient.ProviderDiscord, "31337")
	if err := forPerson.Unlink(ctx, panelclient.ProviderDiscord); err != nil {
		t.Fatalf("Unlink: %v", err)
	}

	// And the link is gone: acting for that account no longer resolves.
	if _, err := forPerson.ListServers(ctx, panelclient.ListServersOptions{}); !errors.Is(err, panelclient.ErrForbidden) {
		t.Fatalf("ListServers after unlinking: %v, want ErrForbidden", err)
	}
}
