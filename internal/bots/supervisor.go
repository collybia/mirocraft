// Package bots runs the chat bots the operator has switched on.
//
// The daemon owns them: a bot is turned on from the panel, not from a shell,
// so something in the daemon has to notice the switch and act on it. That is
// this package.
package bots

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/bots/discord"
	"github.com/collybia/mirocraft/internal/bots/telegram"
	"github.com/collybia/mirocraft/internal/panelclient"
	"github.com/collybia/mirocraft/internal/store"
)

// session is one running bot, whatever the platform.
type session interface {
	Start(ctx context.Context) error
	Stop() error
	Account() string
}

// startTimeout bounds one connection attempt. A platform that is down should
// leave the operator with a failed status, not with a daemon that hangs on
// startup because a chat service is having a bad day.
const startTimeout = 30 * time.Second

// Supervisor keeps the running bots in step with what the panel says.
type Supervisor struct {
	store    *store.Store
	panelURL string
	log      *slog.Logger

	// newClient builds the panel client a bot talks through. A field so tests
	// can hand in a client pointed at their own panel.
	newClient func() (*panelclient.Client, error)

	mu       sync.Mutex
	sessions map[string]session
}

// NewSupervisor returns a supervisor. panelURL is the address the bots reach
// the API at — the loopback one, since they run in the same process.
func NewSupervisor(db *store.Store, panelURL string, newClient func() (*panelclient.Client, error), log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		store:     db,
		panelURL:  panelURL,
		log:       log,
		newClient: newClient,
		sessions:  make(map[string]session),
	}
}

// Sync brings the running bots in line with the stored settings: starts the
// ones switched on, stops the ones switched off, and restarts the ones whose
// token changed.
//
// Called at startup and whenever the settings are saved. Idempotent, because
// the panel calls it on every save and most saves change nothing that matters.
func (s *Supervisor) Sync(ctx context.Context) {
	settings, err := s.store.Bots.List(ctx)
	if err != nil {
		s.log.Error("reading bot settings failed", slog.String("error", err.Error()))
		return
	}

	wanted := make(map[string]*store.BotSettings, len(settings))
	for _, item := range settings {
		if item.Enabled && item.Configured() {
			wanted[item.Provider] = item
		}
	}

	s.mu.Lock()
	running := make([]string, 0, len(s.sessions))
	for provider := range s.sessions {
		running = append(running, provider)
	}
	s.mu.Unlock()

	// Stop first: a token that changed has to have its old session closed
	// before the new one opens, or the platform sees two clients of the same
	// bot and delivers each command to one of them at random.
	for _, provider := range running {
		if _, keep := wanted[provider]; !keep {
			s.stop(ctx, provider)
		}
	}

	for provider, item := range wanted {
		s.ensure(ctx, provider, item)
	}
}

// ensure starts a bot if it is not already running with these settings.
func (s *Supervisor) ensure(ctx context.Context, provider string, settings *store.BotSettings) {
	s.mu.Lock()
	_, already := s.sessions[provider]
	s.mu.Unlock()

	if already {
		// A running session is left alone. Restarting it on every save would
		// disconnect the bot whenever an operator toggled something else.
		return
	}

	s.record(ctx, provider, store.BotStatusConnecting, "", "")

	client, err := s.newClient()
	if err != nil {
		s.fail(ctx, provider, fmt.Errorf("preparing the panel client: %w", err))
		return
	}

	bot, err := s.build(provider, settings.Token, client)
	if err != nil {
		s.fail(ctx, provider, err)
		return
	}

	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	if err := bot.Start(startCtx); err != nil {
		s.fail(ctx, provider, err)
		return
	}

	s.mu.Lock()
	s.sessions[provider] = bot
	s.mu.Unlock()

	s.record(ctx, provider, store.BotStatusConnected, "", bot.Account())
	s.log.Info("bot connected", slog.String("provider", provider), slog.String("account", bot.Account()))
}

// build makes the session for a platform.
func (s *Supervisor) build(provider, token string, client *panelclient.Client) (session, error) {
	switch provider {
	case store.ProviderDiscord:
		return discord.New(token, client, s.panelURL, s.log)
	case store.ProviderTelegram:
		return telegram.New(token, client, s.panelURL, s.log)
	default:
		return nil, fmt.Errorf("bots: %q is not a platform this build supports", provider)
	}
}

// stop closes a running session.
func (s *Supervisor) stop(ctx context.Context, provider string) {
	s.mu.Lock()
	bot, running := s.sessions[provider]
	delete(s.sessions, provider)
	s.mu.Unlock()

	if !running {
		return
	}
	if err := bot.Stop(); err != nil {
		s.log.Warn("stopping a bot failed",
			slog.String("provider", provider), slog.String("error", err.Error()))
	}
	s.record(ctx, provider, store.BotStatusOff, "", "")
	s.log.Info("bot stopped", slog.String("provider", provider))
}

// Restart closes a platform's session and opens it again, which is what a
// changed token needs.
func (s *Supervisor) Restart(ctx context.Context, provider string) {
	s.stop(ctx, provider)
	s.Sync(ctx)
}

// Shutdown closes every running bot.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	providers := make([]string, 0, len(s.sessions))
	for provider := range s.sessions {
		providers = append(providers, provider)
	}
	s.mu.Unlock()

	for _, provider := range providers {
		s.stop(ctx, provider)
	}
}

// Running reports whether a platform's bot is connected right now, which the
// panel shows next to the switch.
func (s *Supervisor) Running(provider string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[provider]
	return ok
}

// fail records why a bot did not start.
//
// The error is stored so the panel can show it: an operator who pasted the
// wrong token should read that where they pasted it, not by finding the
// daemon's log.
func (s *Supervisor) fail(ctx context.Context, provider string, err error) {
	s.log.Error("starting a bot failed",
		slog.String("provider", provider), slog.String("error", err.Error()))
	s.record(ctx, provider, store.BotStatusFailed, humanFailure(err), "")
}

func (s *Supervisor) record(ctx context.Context, provider, status, failure, account string) {
	if err := s.store.Bots.SetStatus(ctx, provider, status, failure, account); err != nil {
		s.log.Warn("recording a bot's status failed",
			slog.String("provider", provider), slog.String("error", err.Error()))
	}
}

// humanFailure turns a connection error into something an operator can act on.
//
// Matched on the message rather than on a type: the chat libraries report a
// rejected token as a transport error, and "websocket: close 4004" says
// nothing to someone who has just pasted a token into a form. Unrecognised
// errors are passed through unchanged — a wrong guess would be worse than the
// library's own words.
func humanFailure(err error) string {
	message := err.Error()
	switch {
	case containsAny(message, "4004", "401", "Unauthorized", "unauthorized"):
		return "Платформа отвергла токен. Проверьте, что скопирован токен бота целиком и что он не был сброшен."
	case containsAny(message, "no such host", "dial tcp", "timeout", "context deadline"):
		return "Не удалось соединиться с платформой. Проверьте, что у сервера есть выход в интернет."
	default:
		return message
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
