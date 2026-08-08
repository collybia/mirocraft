package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/temertika/mirocraft/internal/store"
)

// DefaultSessionTTL is how long a token from a web login stays valid.
const DefaultSessionTTL = 24 * time.Hour

// Options configures an API instance.
type Options struct {
	Store   *store.Store
	Console ConsoleService
	Logger  *slog.Logger

	// TicketTTL overrides the console ticket lifetime. Zero uses TicketTTL.
	TicketTTL time.Duration
	// SessionTTL overrides the login session lifetime.
	SessionTTL time.Duration

	// RateLimit and LoginRateLimit are per-minute allowances. Zero uses the
	// documented defaults.
	RateLimit      int
	LoginRateLimit int

	// CheckOrigin decides which Origin headers may open a WebSocket. Nil keeps
	// gorilla's same-origin default, which is what the embedded panel needs;
	// a deployment serving the panel from another host must set this
	// explicitly rather than silently accepting every origin.
	CheckOrigin func(r *http.Request) bool
}

// API holds the HTTP handlers and their dependencies.
type API struct {
	store   *store.Store
	console ConsoleService
	tickets *TicketStore

	upgrader   websocket.Upgrader
	log        *slog.Logger
	sessionTTL time.Duration

	limiter      *rateLimiter
	loginLimiter *rateLimiter
}

// New builds an API from opts.
func New(opts Options) *API {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	sessionTTL := opts.SessionTTL
	if sessionTTL <= 0 {
		sessionTTL = DefaultSessionTTL
	}
	limit := opts.RateLimit
	if limit <= 0 {
		limit = DefaultRateLimit
	}
	loginLimit := opts.LoginRateLimit
	if loginLimit <= 0 {
		loginLimit = DefaultLoginRateLimit
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}
	if opts.CheckOrigin != nil {
		upgrader.CheckOrigin = opts.CheckOrigin
	}

	return &API{
		store:        opts.Store,
		console:      opts.Console,
		tickets:      NewTicketStore(opts.TicketTTL),
		upgrader:     upgrader,
		log:          log,
		sessionTTL:   sessionTTL,
		limiter:      newRateLimiter(limit, rateWindow),
		loginLimiter: newRateLimiter(loginLimit, rateWindow),
	}
}

// Handler returns the API router mounted under /api/v1.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /api/v1/health", a.handleHealth)
	mux.HandleFunc("GET /api/v1/themes", a.handleListThemes)

	// Login carries its own, much tighter limit, keyed by IP rather than by
	// token: an attacker guessing passwords has no token to key on.
	mux.Handle("POST /api/v1/auth/login", chain(
		http.HandlerFunc(a.handleLogin),
		a.rateLimit(a.loginLimiter, ipKey),
	))

	// Authenticated.
	authed := map[string]http.HandlerFunc{
		"POST /api/v1/auth/logout":                 a.handleLogout,
		"POST /api/v1/auth/refresh":                a.handleRefresh,
		"GET /api/v1/auth/me":                      a.handleMe,
		"GET /api/v1/auth/tokens":                  a.handleListTokens,
		"POST /api/v1/auth/tokens":                 a.handleCreateToken,
		"DELETE /api/v1/auth/tokens/{id}":          a.handleDeleteToken,
		"GET /api/v1/users/me":                     a.handleMe,
		"PATCH /api/v1/users/me":                   a.handlePatchMe,
		"GET /api/v1/users/me/themes":              a.handleListCustomThemes,
		"POST /api/v1/users/me/themes":             a.handleCreateCustomTheme,
		"PATCH /api/v1/users/me/themes/{tid}":      a.handlePatchCustomTheme,
		"DELETE /api/v1/users/me/themes/{tid}":     a.handleDeleteCustomTheme,
		"GET /api/v1/servers/{id}/console/history": a.handleConsoleHistory,
		"POST /api/v1/servers/{id}/command":        a.handleCommand,
		"POST /api/v1/servers/{id}/console/ticket": a.handleConsoleTicket,
	}
	for pattern, handler := range authed {
		mux.Handle(pattern, chain(handler,
			a.rateLimit(a.limiter, tokenKey),
			a.authenticate,
		))
	}

	// Admin.
	admin := map[string]http.HandlerFunc{
		"GET /api/v1/admin/users":         a.handleListUsers,
		"POST /api/v1/admin/users":        a.handleCreateUser,
		"PATCH /api/v1/admin/users/{id}":  a.handlePatchUser,
		"DELETE /api/v1/admin/users/{id}": a.handleDeleteUser,
	}
	for pattern, handler := range admin {
		mux.Handle(pattern, chain(handler,
			a.rateLimit(a.limiter, tokenKey),
			a.authenticate,
			a.requireAdmin,
		))
	}

	// The console socket authenticates with a ticket, so it sits outside the
	// bearer middleware. It is also outside the request rate limit: a single
	// long-lived connection is not a request stream, and docs/API.md says
	// WebSockets do not count.
	mux.HandleFunc("GET /api/v1/servers/{id}/console", a.handleConsoleWS)

	return a.logRequests(mux)
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// Version is stamped at build time via -ldflags.
var Version = "dev"

func (a *API) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: Version})
}

// Shutdown releases API-owned resources. Console subscriptions belong to the
// runner and are released by its own Shutdown.
func (a *API) Shutdown(_ context.Context) error {
	return nil
}
