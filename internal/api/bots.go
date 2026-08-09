package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// BotSupervisor is what the API needs from whatever runs the bots.
//
// An interface rather than the concrete supervisor, so this package does not
// import the chat libraries: the API is the one place every request passes
// through, and its dependencies are worth keeping few.
type BotSupervisor interface {
	// Sync brings the running bots in line with the stored settings.
	Sync(ctx context.Context)
	// Restart closes a platform's session and opens it again, which is what a
	// changed token needs.
	Restart(ctx context.Context, provider string)
	// Running reports whether a platform's bot is connected right now.
	Running(provider string) bool
}

type botResponse struct {
	Provider string `json:"provider"`
	// Configured says a token has been saved. The token itself is never
	// returned: it is a credential the panel holds on the operator's behalf,
	// and an endpoint that hands it back is an endpoint that leaks it.
	Configured bool `json:"configured"`
	// TokenHint is the last four characters, so an operator can tell which of
	// their bots this is.
	TokenHint string    `json:"token_hint"`
	Enabled   bool      `json:"enabled"`
	Running   bool      `json:"running"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Account   string    `json:"account,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type saveBotRequest struct {
	// Token is optional: a nil token leaves the stored one alone, so an
	// operator can flip the switch without pasting the secret again.
	Token   *string `json:"token"`
	Enabled *bool   `json:"enabled"`
}

// handleListBots serves GET /admin/bots.
func (a *API) handleListBots(w http.ResponseWriter, r *http.Request) {
	stored, err := a.store.Bots.List(r.Context())
	if err != nil {
		a.log.Error("listing bot settings failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read bot settings")
		return
	}

	byProvider := make(map[string]*store.BotSettings, len(stored))
	for _, item := range stored {
		byProvider[item.Provider] = item
	}

	// Every supported platform is listed, configured or not: the panel draws a
	// row per platform, and a page that only shows what already exists gives
	// an operator nowhere to paste their first token.
	items := make([]botResponse, 0, 2)
	for _, provider := range []string{store.ProviderDiscord, store.ProviderTelegram} {
		items = append(items, a.botResponseFor(provider, byProvider[provider]))
	}
	writeJSON(w, http.StatusOK, listResponse[botResponse]{Items: items})
}

// handleSaveBot serves PUT /admin/bots/{provider}.
func (a *API) handleSaveBot(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}

	provider, ok := validProvider(w, r)
	if !ok {
		return
	}

	var req saveBotRequest
	if !decodeBody(w, r, &req) {
		return
	}

	current, err := a.store.Bots.Get(r.Context(), provider)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.log.Error("reading bot settings failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read bot settings")
		return
	}

	token := ""
	if current != nil {
		token = current.Token
	}
	tokenChanged := false
	if req.Token != nil {
		trimmed := strings.TrimSpace(*req.Token)
		if trimmed == "" {
			writeFieldError(w, "token", "token must not be empty; delete the settings to forget it")
			return
		}
		tokenChanged = trimmed != token
		token = trimmed
	}
	if token == "" {
		writeFieldError(w, "token", "a token is required before the bot can be switched on")
		return
	}

	enabled := current != nil && current.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	if err := a.store.Bots.Save(r.Context(), provider, token, enabled); err != nil {
		a.log.Error("saving bot settings failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not save bot settings")
		return
	}

	// Audited without the token, obviously — but audited, because handing a
	// bot the ability to act for people is worth a line in the record.
	a.audit(r, principal.UserID, "bot.save."+provider, "", "")

	a.applyBotSettings(r.Context(), provider, tokenChanged)
	a.writeBot(w, provider)
}

// handleDeleteBot serves DELETE /admin/bots/{provider}.
func (a *API) handleDeleteBot(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}

	provider, ok := validProvider(w, r)
	if !ok {
		return
	}

	err := a.store.Bots.Delete(r.Context(), provider)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "no settings for "+provider)
		return
	}
	if err != nil {
		a.log.Error("deleting bot settings failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete bot settings")
		return
	}

	a.audit(r, principal.UserID, "bot.delete."+provider, "", "")
	if a.bots != nil {
		a.bots.Sync(r.Context())
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyBotSettings tells the supervisor to act on what was just saved.
func (a *API) applyBotSettings(ctx context.Context, provider string, tokenChanged bool) {
	if a.bots == nil {
		return
	}
	if tokenChanged {
		// A running session holds the old token, and the platform would keep
		// delivering to it. Restart rather than sync.
		a.bots.Restart(ctx, provider)
		return
	}
	a.bots.Sync(ctx)
}

// writeBot answers with the settings as they now stand.
func (a *API) writeBot(w http.ResponseWriter, provider string) {
	settings, err := a.store.Bots.Get(context.Background(), provider)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.log.Error("reading bot settings failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read bot settings")
		return
	}
	writeJSON(w, http.StatusOK, a.botResponseFor(provider, settings))
}

func (a *API) botResponseFor(provider string, settings *store.BotSettings) botResponse {
	out := botResponse{Provider: provider, Status: store.BotStatusOff}
	if a.bots != nil {
		out.Running = a.bots.Running(provider)
	}
	if settings == nil {
		return out
	}

	out.Configured = settings.Configured()
	out.TokenHint = settings.TokenHint()
	out.Enabled = settings.Enabled
	out.Error = settings.LastError
	out.Account = settings.Account
	out.UpdatedAt = settings.UpdatedAt
	if settings.LastStatus != "" {
		out.Status = settings.LastStatus
	}
	return out
}
