package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// Options configures an API instance.
type Options struct {
	Auth    Authenticator
	Servers ServerLookup
	Console ConsoleService
	Logger  *slog.Logger

	// TicketTTL overrides the console ticket lifetime. Zero uses TicketTTL.
	TicketTTL time.Duration

	// CheckOrigin decides which Origin headers may open a WebSocket. Nil keeps
	// gorilla's same-origin default, which is what the embedded panel needs;
	// a deployment serving the panel from another host must set this
	// explicitly rather than silently accepting every origin.
	CheckOrigin func(r *http.Request) bool
}

// API holds the HTTP handlers and their dependencies.
type API struct {
	auth     Authenticator
	servers  ServerLookup
	console  ConsoleService
	tickets  *TicketStore
	upgrader websocket.Upgrader
	log      *slog.Logger
}

// New builds an API from opts.
func New(opts Options) *API {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
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
		auth:     opts.Auth,
		servers:  opts.Servers,
		console:  opts.Console,
		tickets:  NewTicketStore(opts.TicketTTL),
		upgrader: upgrader,
		log:      log,
	}
}

// Handler returns the API router mounted under /api/v1.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Console. Every route requires the servers:console scope and access to
	// the server; the WebSocket authenticates with a ticket instead, since a
	// browser cannot set an Authorization header on an upgrade request.
	mux.Handle("GET /api/v1/servers/{id}/console/history",
		a.authenticate(http.HandlerFunc(a.handleConsoleHistory)))
	mux.Handle("POST /api/v1/servers/{id}/command",
		a.authenticate(http.HandlerFunc(a.handleCommand)))
	mux.Handle("POST /api/v1/servers/{id}/console/ticket",
		a.authenticate(http.HandlerFunc(a.handleConsoleTicket)))
	mux.HandleFunc("GET /api/v1/servers/{id}/console", a.handleConsoleWS)

	mux.HandleFunc("GET /api/v1/health", a.handleHealth)

	return mux
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
