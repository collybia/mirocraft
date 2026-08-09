package api

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collybia/mirocraft/internal/backup"
	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/dns"
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

	// DNS publishes the records that make servers reachable by name. Nil
	// means the panel is reached by address, which is a supported install.
	DNS DNSPublisher
	// DNSWatcher keeps the panel's own address record current, for the status
	// endpoint to report.
	DNSWatcher *dns.Watcher

	// Certs reports the certificate the panel is served with, so the panel can
	// warn when it is self-signed. Nil means plain HTTP.
	Certs CertStatus

	// Cores is the registry the catalogue asks which loader a server takes.
	Cores *core.Registry
	// Catalog searches and resolves add-ons. Nil disables the catalogue
	// endpoints rather than failing them obscurely.
	Catalog Catalog

	// Events is the bus the panel and the webhooks read. Nil creates one.
	Events *events.Bus

	// Bots runs the chat bots. Nil leaves the settings endpoints working and
	// nothing listening to them, which is what a build without bots looks
	// like — the panel then shows them as switched off rather than lying.
	Bots BotSupervisor

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
	cores       *core.Registry
	catalog     Catalog
	dns         DNSPublisher
	dnsWatcher  *dns.Watcher
	certs       CertStatus
	events      *events.Bus
	bots        BotSupervisor
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
		cores:        opts.Cores,
		catalog:      opts.Catalog,
		dns:          opts.DNS,
		dnsWatcher:   opts.DNSWatcher,
		certs:        opts.Certs,
		events:       bus,
		bots:         opts.Bots,
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

// authedRoutes are the routes behind a bearer token.
func (a *API) authedRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /api/v1/auth/logout":                          a.handleLogout,
		"POST /api/v1/auth/refresh":                         a.handleRefresh,
		"GET /api/v1/auth/me":                               a.handleMe,
		"GET /api/v1/auth/tokens":                           a.handleListTokens,
		"POST /api/v1/auth/tokens":                          a.handleCreateToken,
		"DELETE /api/v1/auth/tokens/{id}":                   a.handleDeleteToken,
		"GET /api/v1/users/me":                              a.handleMe,
		"PATCH /api/v1/users/me":                            a.handlePatchMe,
		"GET /api/v1/users/me/themes":                       a.handleListCustomThemes,
		"POST /api/v1/users/me/themes":                      a.handleCreateCustomTheme,
		"PATCH /api/v1/users/me/themes/{tid}":               a.handlePatchCustomTheme,
		"DELETE /api/v1/users/me/themes/{tid}":              a.handleDeleteCustomTheme,
		"GET /api/v1/servers":                               a.handleListServers,
		"POST /api/v1/servers":                              a.handleCreateServer,
		"GET /api/v1/servers/{id}":                          a.handleGetServer,
		"PATCH /api/v1/servers/{id}":                        a.handlePatchServer,
		"DELETE /api/v1/servers/{id}":                       a.handleDeleteServer,
		"POST /api/v1/servers/{id}/power":                   a.handlePower,
		"GET /api/v1/servers/{id}/tasks":                    a.handleListServerTasks,
		"GET /api/v1/tasks/{id}":                            a.handleGetTask,
		"POST /api/v1/events/ticket":                        a.handleEventsTicket,
		"GET /api/v1/webhooks":                              a.handleListWebhooks,
		"POST /api/v1/webhooks":                             a.handleCreateWebhook,
		"DELETE /api/v1/webhooks/{id}":                      a.handleDeleteWebhook,
		"POST /api/v1/webhooks/{id}/test":                   a.handleTestWebhook,
		"GET /api/v1/servers/{id}/players":                  a.handleListPlayers,
		"POST /api/v1/servers/{id}/players/{name}/kick":     a.handleKickPlayer,
		"POST /api/v1/servers/{id}/players/{name}/ban":      a.handleBanPlayer,
		"DELETE /api/v1/servers/{id}/players/{name}/ban":    a.handleUnbanPlayer,
		"GET /api/v1/servers/{id}/bans":                     a.handleListBans,
		"GET /api/v1/servers/{id}/whitelist":                a.handleGetWhitelist,
		"POST /api/v1/servers/{id}/whitelist":               a.handleAddToWhitelist,
		"PATCH /api/v1/servers/{id}/whitelist":              a.handleSetWhitelistState,
		"DELETE /api/v1/servers/{id}/whitelist/{name}":      a.handleRemoveFromWhitelist,
		"GET /api/v1/servers/{id}/ops":                      a.handleListOps,
		"POST /api/v1/servers/{id}/ops":                     a.handleAddOp,
		"DELETE /api/v1/servers/{id}/ops/{name}":            a.handleRemoveOp,
		"GET /api/v1/servers/{id}/settings":                 a.handleGetSettings,
		"PATCH /api/v1/servers/{id}/settings":               a.handlePatchSettings,
		"GET /api/v1/servers/{id}/backups":                  a.handleListBackups,
		"POST /api/v1/servers/{id}/backups":                 a.handleCreateBackup,
		"GET /api/v1/servers/{id}/backups/schedule":         a.handleGetSchedule,
		"PUT /api/v1/servers/{id}/backups/schedule":         a.handlePutSchedule,
		"GET /api/v1/integrations":                          a.handleListIntegrations,
		"POST /api/v1/integrations/{provider}/code":         a.handleCreateLinkCode,
		"POST /api/v1/integrations/{provider}/link":         a.handleRedeemLink,
		"DELETE /api/v1/integrations/{provider}":            a.handleUnlink,
		"GET /api/v1/dns":                                   a.handleDNSStatus,
		"GET /api/v1/tls":                                   a.handleTLSStatus,
		"GET /api/v1/catalog/search":                        a.handleCatalogSearch,
		"GET /api/v1/catalog/projects/{pid}":                a.handleCatalogProject,
		"GET /api/v1/servers/{id}/catalog":                  a.handleServerContent,
		"POST /api/v1/servers/{id}/catalog/install":         a.handleCatalogInstall,
		"GET /api/v1/servers/{id}/installed":                a.handleListInstalled,
		"DELETE /api/v1/servers/{id}/installed/{file}":      a.handleDeleteInstalled,
		"POST /api/v1/servers/{id}/installed/{file}/toggle": a.handleToggleInstalled,
		"GET /api/v1/servers/{id}/schedules":                a.handleListSchedules,
		"POST /api/v1/servers/{id}/schedules":               a.handleCreateSchedule,
		"PATCH /api/v1/servers/{id}/schedules/{sid}":        a.handlePatchSchedule,
		"DELETE /api/v1/servers/{id}/schedules/{sid}":       a.handleDeleteSchedule,
		"POST /api/v1/servers/{id}/schedules/{sid}/run":     a.handleRunSchedule,
		"GET /api/v1/servers/{id}/backups/{bid}/download":   a.handleDownloadBackup,
		"POST /api/v1/servers/{id}/backups/{bid}/restore":   a.handleRestoreBackup,
		"DELETE /api/v1/servers/{id}/backups/{bid}":         a.handleDeleteBackup,
		"GET /api/v1/servers/{id}/files":                    a.handleListFiles,
		"DELETE /api/v1/servers/{id}/files":                 a.handleDeleteFile,
		"GET /api/v1/servers/{id}/files/content":            a.handleReadFile,
		"PUT /api/v1/servers/{id}/files/content":            a.handleWriteFile,
		"GET /api/v1/servers/{id}/files/download":           a.handleDownloadFile,
		"POST /api/v1/servers/{id}/files/upload":            a.handleUploadFile,
		"POST /api/v1/servers/{id}/files/mkdir":             a.handleMkdir,
		"POST /api/v1/servers/{id}/files/move":              a.handleMoveFile,
		"POST /api/v1/servers/{id}/files/copy":              a.handleCopyFile,
		"POST /api/v1/servers/{id}/files/archive":           a.handleArchive,
		"POST /api/v1/servers/{id}/files/unarchive":         a.handleUnarchive,
		"GET /api/v1/servers/{id}/console/history":          a.handleConsoleHistory,
		"POST /api/v1/servers/{id}/command":                 a.handleCommand,
		"POST /api/v1/servers/{id}/console/ticket":          a.handleConsoleTicket,
	}
}

// adminRoutes are the routes that additionally require the admin role.
func (a *API) adminRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/admin/bots":               a.handleListBots,
		"PUT /api/v1/admin/bots/{provider}":    a.handleSaveBot,
		"DELETE /api/v1/admin/bots/{provider}": a.handleDeleteBot,
		"GET /api/v1/admin/users":              a.handleListUsers,
		"POST /api/v1/admin/users":             a.handleCreateUser,
		"PATCH /api/v1/admin/users/{id}":       a.handlePatchUser,
		"DELETE /api/v1/admin/users/{id}":      a.handleDeleteUser,
	}
}

// publicRoutes need no credentials at all.
func (a *API) publicRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/health":       a.handleHealth,
		"GET /api/v1/themes":       a.handleListThemes,
		"GET /api/v1/openapi.yaml": a.handleOpenAPISpec,
		"GET /api/v1/docs":         a.handleDocs,
		// The docs page's own assets, served from the same handler.
		"GET /api/v1/docs/{asset}": a.handleDocs,
		// Login is public but rate limited by IP; see Handler.
		"POST /api/v1/auth/login": a.handleLogin,
	}
}

// ticketedRoutes authenticate with a one-shot ticket in the query string
// rather than a bearer token, because a browser cannot set a header on a
// WebSocket upgrade.
func (a *API) ticketedRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/servers/{id}/console": a.handleConsoleWS,
		"GET /api/v1/events":               a.handleEventsWS,
	}
}

// AllRoutes returns every route the API serves, as method-and-pattern keys.
//
// Exposed as data rather than only built inline so the contract test can
// compare what the router actually serves against what openapi.yaml claims —
// a spec nobody checks drifts from the code within a release.
func (a *API) AllRoutes() []string {
	var out []string
	for _, group := range []map[string]http.HandlerFunc{
		a.publicRoutes(), a.authedRoutes(), a.adminRoutes(), a.ticketedRoutes(),
	} {
		for pattern := range group {
			out = append(out, pattern)
		}
	}
	sort.Strings(out)
	return out
}

// Handler returns the API router mounted under /api/v1.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public.
	for pattern, handler := range a.publicRoutes() {
		if pattern == "POST /api/v1/auth/login" {
			continue // mounted below with its own limiter
		}
		mux.HandleFunc(pattern, handler)
	}

	// Login carries its own, much tighter limit, keyed by IP rather than by
	// token: an attacker guessing passwords has no token to key on.
	mux.Handle("POST /api/v1/auth/login", chain(
		http.HandlerFunc(a.handleLogin),
		a.rateLimit(a.loginLimiter, ipKey),
	))

	for pattern, handler := range a.authedRoutes() {
		mux.Handle(pattern, chain(handler,
			a.rateLimit(a.limiter, tokenKey),
			a.authenticate,
			// After authenticate, because it rewrites the principal that
			// authenticate established.
			a.withDelegation,
		))
	}

	// Admin. Delegation is refused rather than applied: a chat bot has no
	// business administering accounts, and letting the header through would
	// mean an admin who linked their Discord had handed the bot their role.
	for pattern, handler := range a.adminRoutes() {
		mux.Handle(pattern, chain(handler,
			a.rateLimit(a.limiter, tokenKey),
			a.authenticate,
			a.refuseDelegation,
			a.requireAdmin,
		))
	}

	// The sockets authenticate with a ticket, so they sit outside the bearer
	// middleware. They are also outside the request rate limit: a single
	// long-lived connection is not a request stream, and docs/API.md says
	// WebSockets do not count.
	for pattern, handler := range a.ticketedRoutes() {
		mux.HandleFunc(pattern, handler)
	}

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
