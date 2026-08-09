package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/api"
	"github.com/collybia/mirocraft/internal/bots"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/store"
)

// botAccountEmail is the account the bots' own token belongs to.
//
// A real row rather than a special case in the authentication code: the bots
// go through the API like every other client, and a caller with no account
// would be a hole shaped exactly like the one this design avoids.
const botAccountEmail = "bots@localhost"

// botTokenName marks the token so an operator seeing it in the panel knows
// what it is and does not delete it wondering.
const botTokenName = "внутренний токен ботов" // #nosec G101 -- a label shown in the panel, not a secret

// newBotToken issues the token the in-process bots authenticate with.
//
// A fresh one on every start, with the previous ones revoked: the bots run in
// this process and need the plaintext, so a token that survived a restart
// would have to be stored somewhere readable. Minting it at startup and
// keeping it in memory means there is no long-lived secret on disk at all.
//
// The scope is integrations:act and nothing else. On its own that grants
// nothing — it only allows acting for an account that has been linked, and
// what such a request may do is capped by the API.
func newBotToken(ctx context.Context, db *store.Store, log *slog.Logger) (string, error) {
	user, err := db.Users.GetByEmail(ctx, botAccountEmail)
	if err != nil {
		user = &store.User{
			Email: botAccountEmail,
			// No password anyone could use: this account exists to own a
			// token, and logging into it is not a thing that should work.
			PasswordHash: "-",
			Role:         store.RoleUser,
			// Blocked would be tidier, but a blocked user's token is refused,
			// and this one has to work.
		}
		if err := db.Users.Create(ctx, user); err != nil {
			return "", fmt.Errorf("creating the bots' account: %w", err)
		}
	}

	// The previous run's tokens are revoked rather than reused: this one
	// cannot know their plaintext, so they are dead weight that would
	// accumulate one row per restart.
	if err := db.Tokens.DeleteByUserAndName(ctx, user.ID, botTokenName); err != nil {
		log.Warn("clearing the bots' previous tokens failed", slog.String("error", err.Error()))
	}

	value, hash, err := store.GenerateToken()
	if err != nil {
		return "", fmt.Errorf("generating the bots' token: %w", err)
	}
	if err := db.Tokens.Create(ctx, &store.Token{
		UserID: user.ID, Name: botTokenName, Hash: hash,
		Scopes: []string{api.ScopeIntegrationsAct}, Kind: store.TokenKindAPI,
	}); err != nil {
		return "", fmt.Errorf("storing the bots' token: %w", err)
	}
	return value, nil
}

// loopbackURL is the address the in-process bots reach the API at.
//
// Loopback rather than the panel's public name: the request never leaves the
// machine, so it needs neither DNS nor a certificate that resolves. A listener
// bound to every interface is reached at 127.0.0.1; one bound to a single
// address is reached at that address.
func loopbackURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// publicPanelURL is the address the bots tell people to open, which is the one
// that works from outside — unlike the loopback address they use themselves.
func publicPanelURL(scheme, addr, domain string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = "8080"
	}
	host := domain
	if strings.TrimSpace(host) == "" {
		// Nothing better to offer: an operator without a domain reaches the
		// panel by address, and so do their users.
		host = "127.0.0.1"
	}

	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// startBots issues the internal token, builds the supervisor and brings up
// whatever the operator has switched on.
//
// A failure here is logged and not fatal: a panel that refuses to start
// because Discord is unreachable would be a panel that stops managing
// Minecraft servers over a chat outage.
func startBots(ctx context.Context, db *store.Store, listenAddr, panelURL string, log *slog.Logger) *bots.Supervisor {
	token, err := newBotToken(ctx, db, log)
	if err != nil {
		log.Error("preparing the bots' token failed; bots will not run",
			slog.String("error", err.Error()))
		return nil
	}

	loopback := loopbackURL(listenAddr)
	newClient := func() (*panelclient.Client, error) {
		return panelclient.New(loopback, token, panelclient.WithUserAgent("mirocraft-bots"))
	}

	supervisor := bots.NewSupervisor(db, panelURL, newClient, log)

	// In the background: connecting to a chat platform takes seconds, and the
	// panel should be answering before then.
	go func() {
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		supervisor.Sync(syncCtx)
	}()

	return supervisor
}

// botSupervisorOrNil hands the API a typed nil-free interface value.
//
// Assigning a nil *bots.Supervisor to an interface makes an interface that is
// not nil, and every "if a.bots != nil" downstream would then call methods on
// a nil receiver. This is the one place that mistake can be made, so it is
// made impossible here rather than guarded against everywhere.
func botSupervisorOrNil(s *bots.Supervisor) api.BotSupervisor {
	if s == nil {
		return nil
	}
	return s
}
