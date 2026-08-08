package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collybia/mirocraft/internal/backup"
	"github.com/collybia/mirocraft/internal/events"
	"github.com/collybia/mirocraft/internal/store"
)

// Defaults applied when Options leaves a field zero.
const (
	// DefaultSessionTTL is how long a token from a web login stays valid.
	DefaultSessionTTL = 24 * time.Hour
	// DefaultPortFrom and DefaultPortTo bound automatic port assignment,
	// starting at the vanilla default port.
	DefaultPortFrom = 25565
	DefaultPortTo   = 25665
	// DefaultStopTimeout is how long a graceful stop may take.
	DefaultStopTimeout = 60 * time.Second
)

// Options configures an API instance.
type Options struct {
	Store       *store.Store
	Console     ConsoleService
	Lifecycle   Lifecycle
	Provisioner Provisioner
	Logger      *slog.Logger

	// Ping overrides how a running server is asked for its player counts.
	// Nil uses the real Server List Ping.
	Ping Pinger

	// Backups archives and restores server directories. Nil disables the
	// backup endpoints rather than failing them obscurely.
	Backups *backup.Manager

	// Events is the bus the panel and the webhooks read. Nil creates one.
	Events *events.Bus

	// DataDir is where server directories are created.
	DataDir string
	// PortFrom and PortTo bound automatic port assignment.
	PortFrom int
	PortTo   int
	// StopTimeout is the default graceful-stop budget for power actions.
	StopTimeout time.Duration

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
	store       *store.Store
	console     ConsoleService
	lifecycle   Lifecycle
	provisioner Provisioner
	backups     *backup.Manager
	events      *events.Bus
	ping        Pinger
	tickets     *TicketStore
	tasks       *taskRegistry

	// scheduleRuns guards a chain against overlapping itself; a chain that
	// waits can outlast the tick that started it.
	scheduleRuns *runningSchedules

	dataDir     string
	portFrom    int
	portTo      int
	stopTimeout time.Duration

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

	bus := opts.Events
	if bus == nil {
		bus = events.NewBus()
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}
	if opts.CheckOrigin != nil {
		upgrader.CheckOrigin = opts.CheckOrigin
	}

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = "./data"
	}
	portFrom, portTo := opts.PortFrom, opts.PortTo
	if portFrom <= 0 || portTo < portFrom {
		portFrom, portTo = DefaultPortFrom, DefaultPortTo
	}
	stopTimeout := opts.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = DefaultStopTimeout
	}

	a := &API{
		store:        opts.Store,
		console:      opts.Console,
		lifecycle:    opts.Lifecycle,
		provisioner:  opts.Provisioner,
		ping:         opts.Ping,
		backups:      opts.Backups,
		events:       bus,
		tickets:      NewTicketStore(opts.TicketTTL),
		tasks:        newTaskRegistry(),
		scheduleRuns: newRunningSchedules(),
		dataDir:      dataDir,
		portFrom:     portFrom,
		portTo:       portTo,
		stopTimeout:  stopTimeout,
		upgrader:     upgrader,
		log:          log,
		sessionTTL:   sessionTTL,
		limiter:      newRateLimiter(limit, rateWindow),
		loginLimiter: newRateLimiter(loginLimit, rateWindow),
	}

	// A finished task is worth knowing about: the panel can stop polling and
	// a webhook can report the outcome.
	a.tasks.onFinish = func(task Task) {
		a.events.Publish(events.Event{
			Type:     events.TypeTaskUpdated,
			ServerID: task.ServerID,
			OwnerID:  a.ownerOf(task.ServerID),
			Data: map[string]any{
				"task_id": task.ID, "kind": task.Kind,
				"status": task.Status, "progress": task.Progress,
				"error": task.Error,
			},
		})
	}

	return a
}

// ownerOf resolves who a server belongs to, for routing events.
func (a *API) ownerOf(serverID string) string {
	if serverID == "" || a.store == nil {
		return ""
	}
	server, err := a.store.Servers.GetByID(context.Background(), serverID)
	if err != nil {
		return ""
	}
	return server.OwnerID
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
		"POST /api/v1/auth/logout":                        a.handleLogout,
		"POST /api/v1/auth/refresh":                       a.handleRefresh,
		"GET /api/v1/auth/me":                             a.handleMe,
		"GET /api/v1/auth/tokens":                         a.handleListTokens,
		"POST /api/v1/auth/tokens":                        a.handleCreateToken,
		"DELETE /api/v1/auth/tokens/{id}":                 a.handleDeleteToken,
		"GET /api/v1/users/me":                            a.handleMe,
		"PATCH /api/v1/users/me":                          a.handlePatchMe,
		"GET /api/v1/users/me/themes":                     a.handleListCustomThemes,
		"POST /api/v1/users/me/themes":                    a.handleCreateCustomTheme,
		"PATCH /api/v1/users/me/themes/{tid}":             a.handlePatchCustomTheme,
		"DELETE /api/v1/users/me/themes/{tid}":            a.handleDeleteCustomTheme,
		"GET /api/v1/servers":                             a.handleListServers,
		"POST /api/v1/servers":                            a.handleCreateServer,
		"GET /api/v1/servers/{id}":                        a.handleGetServer,
		"PATCH /api/v1/servers/{id}":                      a.handlePatchServer,
		"DELETE /api/v1/servers/{id}":                     a.handleDeleteServer,
		"POST /api/v1/servers/{id}/power":                 a.handlePower,
		"GET /api/v1/servers/{id}/tasks":                  a.handleListServerTasks,
		"GET /api/v1/tasks/{id}":                          a.handleGetTask,
		"POST /api/v1/events/ticket":                      a.handleEventsTicket,
		"GET /api/v1/webhooks":                            a.handleListWebhooks,
		"POST /api/v1/webhooks":                           a.handleCreateWebhook,
		"DELETE /api/v1/webhooks/{id}":                    a.handleDeleteWebhook,
		"POST /api/v1/webhooks/{id}/test":                 a.handleTestWebhook,
		"GET /api/v1/servers/{id}/players":                a.handleListPlayers,
		"POST /api/v1/servers/{id}/players/{name}/kick":   a.handleKickPlayer,
		"POST /api/v1/servers/{id}/players/{name}/ban":    a.handleBanPlayer,
		"DELETE /api/v1/servers/{id}/players/{name}/ban":  a.handleUnbanPlayer,
		"GET /api/v1/servers/{id}/bans":                   a.handleListBans,
		"GET /api/v1/servers/{id}/whitelist":              a.handleGetWhitelist,
		"POST /api/v1/servers/{id}/whitelist":             a.handleAddToWhitelist,
		"PATCH /api/v1/servers/{id}/whitelist":            a.handleSetWhitelistState,
		"DELETE /api/v1/servers/{id}/whitelist/{name}":    a.handleRemoveFromWhitelist,
		"GET /api/v1/servers/{id}/ops":                    a.handleListOps,
		"POST /api/v1/servers/{id}/ops":                   a.handleAddOp,
		"DELETE /api/v1/servers/{id}/ops/{name}":          a.handleRemoveOp,
		"GET /api/v1/servers/{id}/settings":               a.handleGetSettings,
		"PATCH /api/v1/servers/{id}/settings":             a.handlePatchSettings,
		"GET /api/v1/servers/{id}/backups":                a.handleListBackups,
		"POST /api/v1/servers/{id}/backups":               a.handleCreateBackup,
		"GET /api/v1/servers/{id}/backups/schedule":       a.handleGetSchedule,
		"PUT /api/v1/servers/{id}/backups/schedule":       a.handlePutSchedule,
		"GET /api/v1/servers/{id}/schedules":              a.handleListSchedules,
		"POST /api/v1/servers/{id}/schedules":             a.handleCreateSchedule,
		"PATCH /api/v1/servers/{id}/schedules/{sid}":      a.handlePatchSchedule,
		"DELETE /api/v1/servers/{id}/schedules/{sid}":     a.handleDeleteSchedule,
		"POST /api/v1/servers/{id}/schedules/{sid}/run":   a.handleRunSchedule,
		"GET /api/v1/servers/{id}/backups/{bid}/download": a.handleDownloadBackup,
		"POST /api/v1/servers/{id}/backups/{bid}/restore": a.handleRestoreBackup,
		"DELETE /api/v1/servers/{id}/backups/{bid}":       a.handleDeleteBackup,
		"GET /api/v1/servers/{id}/files":                  a.handleListFiles,
		"DELETE /api/v1/servers/{id}/files":               a.handleDeleteFile,
		"GET /api/v1/servers/{id}/files/content":          a.handleReadFile,
		"PUT /api/v1/servers/{id}/files/content":          a.handleWriteFile,
		"GET /api/v1/servers/{id}/files/download":         a.handleDownloadFile,
		"POST /api/v1/servers/{id}/files/upload":          a.handleUploadFile,
		"POST /api/v1/servers/{id}/files/mkdir":           a.handleMkdir,
		"POST /api/v1/servers/{id}/files/move":            a.handleMoveFile,
		"POST /api/v1/servers/{id}/files/copy":            a.handleCopyFile,
		"POST /api/v1/servers/{id}/files/archive":         a.handleArchive,
		"POST /api/v1/servers/{id}/files/unarchive":       a.handleUnarchive,
		"GET /api/v1/servers/{id}/console/history":        a.handleConsoleHistory,
		"POST /api/v1/servers/{id}/command":               a.handleCommand,
		"POST /api/v1/servers/{id}/console/ticket":        a.handleConsoleTicket,
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
	// The event bus authenticates with a ticket for the same reason.
	mux.HandleFunc("GET /api/v1/events", a.handleEventsWS)

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

// Events exposes the bus, so the daemon can run the webhook dispatcher on it.
func (a *API) Events() *events.Bus { return a.events }

// Shutdown releases API-owned resources. Console subscriptions belong to the
// runner and are released by its own Shutdown.
func (a *API) Shutdown(_ context.Context) error {
	a.events.Close()
	return nil
}
