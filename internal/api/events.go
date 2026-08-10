package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collybia/mirocraft/internal/events"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

// MaxWebhooksPerUser bounds how many endpoints one account can register, so a
// single user cannot turn the daemon into an outbound request generator.
const MaxWebhooksPerUser = 20

// --- wire types ---

type webhookResponse struct {
	ID      string   `json:"id"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`

	// The secret is never returned. It is stored in the clear because signing
	// requires it, which makes it worth not handing back out.
	HasSecret bool `json:"has_secret"`

	LastStatus    *int       `json:"last_status"`
	LastError     string     `json:"last_error,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	FailureCount  int        `json:"failure_count"`
	CreatedAt     time.Time  `json:"created_at"`
}

type createWebhookRequest struct {
	URL     string   `json:"url"`
	Secret  string   `json:"secret"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
}

type eventTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

func toWebhookResponse(w *store.Webhook) webhookResponse {
	return webhookResponse{
		ID: w.ID, URL: w.URL, Events: w.Events, Enabled: w.Enabled,
		HasSecret:     w.Secret != "",
		LastStatus:    w.LastStatus,
		LastError:     w.LastError,
		LastAttemptAt: w.LastAttemptAt,
		LastSuccessAt: w.LastSuccessAt,
		FailureCount:  w.FailureCount,
		CreatedAt:     w.CreatedAt,
	}
}

// --- the events socket ---

// handleEventsTicket serves POST /events/ticket.
//
// The same one-shot ticket the console uses, for the same reason: a browser
// cannot set an Authorization header on an upgrade, and a token in a URL ends
// up in proxy logs and browser history.
func (a *API) handleEventsTicket(w http.ResponseWriter, r *http.Request) {
	// The bus reports what servers are doing, so watching it is a read of the
	// same facts GET /servers returns.
	principal, ok := requireScope(w, r, ScopeServersRead)
	if !ok {
		return
	}

	// An empty server id marks this as a bus ticket rather than a console one.
	token, expiresAt, err := a.tickets.Issue("", principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not issue a ticket")
		return
	}

	writeJSON(w, http.StatusCreated, eventTicketResponse{Ticket: token, ExpiresAt: expiresAt})
}

// handleEventsWS serves GET /events?token={ticket}.
func (a *API) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	ticket, err := a.tickets.Redeem(r.URL.Query().Get("token"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized,
			"invalid, expired or already used ticket")
		return
	}
	// A console ticket carries a server id; using one here would let a client
	// with console access to one server watch the whole account's events.
	if ticket.ServerID != "" {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"this ticket is for a console, not the event bus")
		return
	}

	user, err := a.store.Users.GetByID(r.Context(), ticket.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "user no longer exists")
		return
	}

	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		a.log.Debug("events websocket upgrade failed", slog.String("error", err.Error()))
		return
	}

	a.runEventsSocket(r.Context(), conn, user)
}

// runEventsSocket streams events until either side goes away.
func (a *API) runEventsSocket(parent context.Context, conn *websocket.Conn, user *store.User) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	defer func() { _ = conn.Close() }()

	stream, unsubscribe := a.events.Subscribe(ctx, user.ID, user.IsAdmin())
	defer unsubscribe()

	outbound := make(chan any, outboundQueue)
	writerDone := make(chan struct{})
	go a.writePump(conn, outbound, writerDone)

	// Same shape as the console socket: the sender is waited on before the
	// channel closes, because closing it under a live sender panics.
	var fanOut sync.WaitGroup
	fanOut.Add(1)
	go func() {
		defer fanOut.Done()
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-stream:
				if !ok {
					return
				}
				trySend(outbound, event)
			}
		}
	}()

	// The bus is one-way, but the read side still has to run: it is what
	// notices a dropped connection and answers pings.
	a.drainEventsSocket(conn)

	cancel()
	fanOut.Wait()
	close(outbound)
	<-writerDone
}

// drainEventsSocket reads and discards client frames until the socket closes.
func (a *API) drainEventsSocket(conn *websocket.Conn) {
	conn.SetReadLimit(4 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// --- webhook endpoints ---

// handleListWebhooks serves GET /webhooks.
func (a *API) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireScope(w, r, ScopeServersRead)
	if !ok {
		return
	}

	hooks, err := a.store.Webhooks.ListByUser(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list webhooks")
		return
	}

	items := make([]webhookResponse, 0, len(hooks))
	for _, hook := range hooks {
		items = append(items, toWebhookResponse(hook))
	}
	writeJSON(w, http.StatusOK, listResponse[webhookResponse]{Items: items})
}

// handleCreateWebhook serves POST /webhooks.
func (a *API) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	// Registering an endpoint the daemon will make outbound requests to is a
	// change to the account, not a read of it — a read-only bot token must not
	// be able to do it.
	principal, ok := requireScope(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	var req createWebhookRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := validateWebhookURL(req.URL); err != nil {
		writeFieldError(w, "url", err.Error())
		return
	}
	if err := validateEventTypes(req.Events); err != nil {
		writeFieldError(w, "events", err.Error())
		return
	}
	// A hook with no secret cannot be verified by its receiver, which defeats
	// the point of signing, so one is required rather than optional.
	if len(req.Secret) < 8 {
		writeFieldError(w, "secret", "a secret of at least 8 characters is required")
		return
	}

	existing, err := a.store.Webhooks.ListByUser(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read webhooks")
		return
	}
	if len(existing) >= MaxWebhooksPerUser {
		writeFieldError(w, "url", "you already have the maximum number of webhooks")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	hook := &store.Webhook{
		UserID: principal.UserID, URL: req.URL, Secret: req.Secret,
		Events: req.Events, Enabled: enabled,
	}
	if err := a.store.Webhooks.Create(r.Context(), hook); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create the webhook")
		return
	}

	a.audit(r, principal.UserID, "webhook.create", hook.ID, hook.URL)
	writeJSON(w, http.StatusCreated, toWebhookResponse(hook))
}

// handleDeleteWebhook serves DELETE /webhooks/{id}.
func (a *API) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireScope(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	hook, ok := a.ownedWebhook(w, r, principal)
	if !ok {
		return
	}
	if err := a.store.Webhooks.Delete(r.Context(), hook.ID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete the webhook")
		return
	}

	a.audit(r, principal.UserID, "webhook.delete", hook.ID, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleTestWebhook serves POST /webhooks/{id}/test.
func (a *API) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireScope(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	hook, ok := a.ownedWebhook(w, r, principal)
	if !ok {
		return
	}

	// Published on the bus rather than delivered directly, so the test takes
	// exactly the path a real event does — including the signing, the retries
	// and the private-address check.
	a.events.Publish(events.Event{
		Type:    events.TypeTaskUpdated,
		OwnerID: principal.UserID,
		Data: map[string]any{
			"test":       true,
			"webhook_id": hook.ID,
			"message":    "This is a test delivery from Mirocraft",
		},
	})

	a.audit(r, principal.UserID, "webhook.test", hook.ID, "")
	w.WriteHeader(http.StatusAccepted)
}

// ownedWebhook loads the hook named in the path and checks ownership.
func (a *API) ownedWebhook(w http.ResponseWriter, r *http.Request, principal *Principal) (*store.Webhook, bool) {
	hook, err := a.store.Webhooks.GetByID(r.Context(), r.PathValue("id"))
	// Another user's hook is reported as missing rather than forbidden, so
	// ids cannot be probed.
	if err != nil || (hook.UserID != principal.UserID && !principal.IsAdmin()) {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "webhook not found")
		return nil, false
	}
	return hook, true
}

func validateWebhookURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errNoURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errBadURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errBadScheme
	}
	if parsed.Host == "" {
		return errBadURL
	}
	return nil
}

func validateEventTypes(types []string) error {
	for _, t := range types {
		if !events.IsKnownType(t) {
			return errUnknownEvent(t)
		}
	}
	return nil
}

// --- the source the dispatcher reads ---

// WebhookSource adapts the store to what the dispatcher needs, so the daemon
// can wire the two together without either knowing about the other.
func WebhookSource(s *store.Store) events.TargetSource { return webhookSource{store: s} }

// webhookSource adapts the store to what the dispatcher needs.
type webhookSource struct {
	store *store.Store
}

// TargetsFor returns the hooks that should receive an event.
func (s webhookSource) TargetsFor(ctx context.Context, event events.Event) ([]events.Target, error) {
	hooks, err := s.store.Webhooks.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}

	var out []events.Target
	for _, hook := range hooks {
		// An event belongs to the owner of the server it came from, so a hook
		// only ever sees its own user's servers.
		if event.OwnerID != "" && hook.UserID != event.OwnerID {
			continue
		}
		if !hook.Wants(event.Type) {
			continue
		}
		out = append(out, events.Target{ID: hook.ID, URL: hook.URL, Secret: hook.Secret})
	}
	return out, nil
}

// --- emitting events ---

// WatchServer republishes a running server's console and status onto the bus.
//
// Started when a server starts and torn down when its subscriptions close, so
// there is one watcher per running server rather than a poller per client.
func (a *API) WatchServer(ctx context.Context, serverID, ownerID string) {
	if a.events == nil || a.console == nil {
		return
	}

	statuses, unsubscribeStatus, err := a.console.SubscribeStatus(ctx, serverID)
	if err != nil {
		return
	}
	lines, unsubscribeLines, err := a.console.Subscribe(ctx, serverID)
	if err != nil {
		unsubscribeStatus()
		return
	}

	go func() {
		defer unsubscribeStatus()
		defer unsubscribeLines()

		for {
			select {
			case <-ctx.Done():
				return

			case status, ok := <-statuses:
				if !ok {
					return
				}
				a.emitStatus(serverID, ownerID, status)

			case line, ok := <-lines:
				if !ok {
					return
				}
				a.emitFromConsole(serverID, ownerID, line.Text)
			}
		}
	}()
}

func (a *API) emitStatus(serverID, ownerID string, status runner.Status) {
	a.events.Publish(events.Event{
		Type:     events.TypeServerStatusChanged,
		ServerID: serverID,
		OwnerID:  ownerID,
		Data:     map[string]any{"status": string(status)},
	})

	if crashed(status) {
		a.events.Publish(events.Event{
			Type:     events.TypeServerCrashed,
			ServerID: serverID,
			OwnerID:  ownerID,
		})
		// The one place that already knows a server died. Bringing it back is
		// what the auto_restart switch on its settings page promises.
		a.autoRestart(serverID)
	}
}

// emitFromConsole turns console lines into player events.
func (a *API) emitFromConsole(serverID, ownerID, text string) {
	name, joined, ok := parsePlayerLine(text)
	if !ok {
		return
	}

	eventType := events.TypePlayerLeft
	if joined {
		eventType = events.TypePlayerJoined
	}
	a.events.Publish(events.Event{
		Type:     eventType,
		ServerID: serverID,
		OwnerID:  ownerID,
		Data:     map[string]any{"name": name},
	})
}
