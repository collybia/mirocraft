package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collybia/mirocraft/internal/events"
	"github.com/collybia/mirocraft/internal/runner"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tickets and the socket ---

func (e *testEnv) issueEventsTicket(token string) string {
	e.t.Helper()

	resp := e.do(http.MethodPost, "/api/v1/events/ticket", nil, token)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("issuing an events ticket: status %d", resp.StatusCode)
	}
	return decodeJSON[eventTicketResponse](e.t, resp).Ticket
}

func (e *testEnv) dialEvents(ticket string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()

	u, err := url.Parse(e.server.URL)
	if err != nil {
		e.t.Fatalf("parsing test server url: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/api/v1/events"
	u.RawQuery = "token=" + url.QueryEscape(ticket)

	return websocket.DefaultDialer.Dial(u.String(), nil)
}

func TestEventsSocketDeliversPublishedEvents(t *testing.T) {
	env := newTestEnv(t)

	conn, _, err := env.dialEvents(env.issueEventsTicket(env.token))
	if err != nil {
		t.Fatalf("dialling the event socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The subscription is set up inside the handler, which runs concurrently
	// with the dial returning, so the publish has to be retried until it lands.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				env.api.Events().Publish(events.Event{
					Type:     events.TypeServerStatusChanged,
					ServerID: testServerID,
					OwnerID:  env.user.ID,
					Data:     map[string]any{"status": "running"},
				})
			}
		}
	}()

	frame := readUntil(t, conn, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == events.TypeServerStatusChanged
	})

	if frame["server_id"] != testServerID {
		t.Errorf("server_id = %v", frame["server_id"])
	}
	if _, present := frame["owner_id"]; present {
		t.Error("the frame carries the owner id")
	}
}

// An event for someone else's server must not reach this socket.
func TestEventsSocketIsScopedToTheUser(t *testing.T) {
	env := newTestEnv(t)

	conn, _, err := env.dialEvents(env.issueEventsTicket(env.token))
	if err != nil {
		t.Fatalf("dialling the event socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	time.Sleep(200 * time.Millisecond) // let the handler subscribe
	env.api.Events().Publish(events.Event{
		Type:     events.TypeServerCrashed,
		ServerID: otherServerID,
		OwnerID:  env.other.ID,
	})
	// A second event the socket *should* see, so the test can tell "not yet"
	// from "never" without waiting out a long timeout.
	env.api.Events().Publish(events.Event{
		Type:    events.TypeTaskUpdated,
		OwnerID: env.user.ID,
	})

	frame := readUntil(t, conn, 5*time.Second, func(map[string]any) bool { return true })
	if frame["type"] != events.TypeTaskUpdated {
		t.Fatalf("the first frame was %v, want another user's event to have been skipped", frame["type"])
	}
}

// A console ticket names a server; redeeming one on the bus would turn console
// access to a single server into a view of the whole account.
func TestConsoleTicketIsRejectedOnTheEventSocket(t *testing.T) {
	env := newTestEnv(t)

	consoleTicket := env.issueTicket(testServerID)

	conn, resp, err := env.dialEvents(consoleTicket)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a console ticket was accepted on the event socket")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

func TestEventsTicketIsOneShot(t *testing.T) {
	env := newTestEnv(t)

	ticket := env.issueEventsTicket(env.token)

	conn, _, err := env.dialEvents(ticket)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	_ = conn.Close()

	second, resp, err := env.dialEvents(ticket)
	if err == nil {
		_ = second.Close()
		t.Fatal("a ticket was redeemed twice")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func TestEventsTicketNeedsTheReadScope(t *testing.T) {
	env := newTestEnv(t)

	// A token that can write files but knows nothing about servers.
	narrow := env.mintToken(env.user.ID, []string{ScopeFilesWrite})

	resp := env.do(http.MethodPost, "/api/v1/events/ticket", nil, narrow)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeForbidden {
		t.Errorf("code = %q", code)
	}
}

// --- webhook CRUD ---

func TestWebhookCreateAndList(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	resp := env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url":    "https://example.com/hook",
		"secret": "a-long-enough-secret",
		"events": []string{events.TypeServerCrashed, events.TypeBackupFailed},
	}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	created := decodeJSON[webhookResponse](t, resp)
	if created.ID == "" {
		t.Fatal("the created webhook has no id")
	}
	if !created.HasSecret {
		t.Error("has_secret = false")
	}
	if !created.Enabled {
		t.Error("a webhook was created disabled by default")
	}

	list := decodeJSON[listResponse[webhookResponse]](t,
		env.do(http.MethodGet, "/api/v1/webhooks", nil, token))
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Items)
	}
}

// The secret is stored in the clear because signing needs it, which is exactly
// why it must never come back out over the API.
func TestWebhookSecretIsNeverReturned(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	const secret = "sup3r-secret-value"
	create := env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://example.com/hook", "secret": secret,
		"events": []string{events.TypeServerCrashed},
	}, token)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("creating: status %d", create.StatusCode)
	}

	for _, resp := range []*http.Response{create,
		env.do(http.MethodGet, "/api/v1/webhooks", nil, token)} {

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading the response: %v", err)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("a response carried the secret: %s", body)
		}
	}
}

func TestWebhookValidation(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no url", map[string]any{"secret": "long-enough-secret", "events": []string{events.TypeServerCrashed}}},
		{"a file url", map[string]any{"url": "file:///etc/passwd", "secret": "long-enough-secret", "events": []string{}}},
		{"no host", map[string]any{"url": "https://", "secret": "long-enough-secret", "events": []string{}}},
		{"a short secret", map[string]any{"url": "https://example.com/h", "secret": "abc", "events": []string{}}},
		{"no secret", map[string]any{"url": "https://example.com/h", "events": []string{}}},
		// Subscribing to a typo would silently deliver nothing, which is worse
		// than being told the type does not exist.
		{"an unknown event", map[string]any{"url": "https://example.com/h", "secret": "long-enough-secret",
			"events": []string{"server.exploded"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.do(http.MethodPost, "/api/v1/webhooks", tc.body, token)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestWebhookCreateNeedsTheWriteScope(t *testing.T) {
	env := newTestEnv(t)
	readOnly := env.mintToken(env.user.ID, []string{ScopeServersRead})

	resp := env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://example.com/hook", "secret": "a-long-enough-secret",
		"events": []string{events.TypeServerCrashed},
	}, readOnly)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// Another user's webhook is reported missing rather than forbidden, so ids
// cannot be probed.
func TestWebhookOfAnotherUserIsNotFound(t *testing.T) {
	env := newTestEnv(t)
	mine := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	theirs := env.mintToken(env.other.ID, []string{ScopeServersRead, ScopeServersWrite})

	created := decodeJSON[webhookResponse](t, env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://example.com/hook", "secret": "a-long-enough-secret",
		"events": []string{events.TypeServerCrashed},
	}, mine))

	for _, call := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/webhooks/" + created.ID},
		{http.MethodPost, "/api/v1/webhooks/" + created.ID + "/test"},
	} {
		resp := env.do(call.method, call.path, nil, theirs)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", call.method, call.path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// And the owner's own list is untouched by the attempt.
	list := decodeJSON[listResponse[webhookResponse]](t,
		env.do(http.MethodGet, "/api/v1/webhooks", nil, mine))
	if len(list.Items) != 1 {
		t.Fatalf("the webhook is gone after another user tried to delete it")
	}
}

func TestWebhookDelete(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	created := decodeJSON[webhookResponse](t, env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://example.com/hook", "secret": "a-long-enough-secret",
		"events": []string{events.TypeServerCrashed},
	}, token))

	resp := env.do(http.MethodDelete, "/api/v1/webhooks/"+created.ID, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	list := decodeJSON[listResponse[webhookResponse]](t,
		env.do(http.MethodGet, "/api/v1/webhooks", nil, token))
	if len(list.Items) != 0 {
		t.Fatalf("the webhook survived deletion: %+v", list.Items)
	}
}

func TestWebhookLimitPerUser(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	for i := 0; i < MaxWebhooksPerUser; i++ {
		resp := env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
			"url": "https://example.com/hook", "secret": "a-long-enough-secret",
			"events": []string{events.TypeServerCrashed},
		}, token)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("webhook %d: status %d", i, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	resp := env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://example.com/one-too-many", "secret": "a-long-enough-secret",
		"events": []string{events.TypeServerCrashed},
	}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 past the limit", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- an event's whole path, from the bus to a signed request ---

func TestWebhookDeliveryIsSignedEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	const secret = "an-end-to-end-secret"

	type delivery struct {
		signature string
		body      []byte
	}
	received := make(chan delivery, 4)

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- delivery{signature: r.Header.Get(events.SignatureHeader), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	created := decodeJSON[webhookResponse](t, env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": receiver.URL, "secret": secret,
		"events": []string{events.TypeTaskUpdated},
	}, token))

	// The daemon wires this up in main; the test does the same by hand.
	dispatcher := events.NewDispatcher(WebhookSource(env.db), env.db.Webhooks, silentLogger())
	dispatcher.Client = receiver.Client()
	dispatcher.AllowPrivateHosts = true // httptest listens on loopback

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx, env.api.Events())
	time.Sleep(100 * time.Millisecond) // let the dispatcher subscribe

	resp := env.do(http.MethodPost, "/api/v1/webhooks/"+created.ID+"/test", nil, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("triggering the test delivery: status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	select {
	case got := <-received:
		if !events.Verify(secret, got.body, got.signature) {
			t.Fatalf("the delivery signature does not verify: %q", got.signature)
		}
		var event events.Event
		if err := json.Unmarshal(got.body, &event); err != nil {
			t.Fatalf("the body is not an event: %v", err)
		}
		if event.Type != events.TypeTaskUpdated {
			t.Errorf("delivered %q", event.Type)
		}
		if event.Data["webhook_id"] != created.ID {
			t.Errorf("data = %v", event.Data)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the test delivery never arrived")
	}
}

// A hook that did not subscribe to a type must not receive it.
func TestWebhookOnlyReceivesSubscribedTypes(t *testing.T) {
	env := newTestEnv(t)
	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})

	var deliveries int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&deliveries, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	resp := env.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": receiver.URL, "secret": "a-long-enough-secret",
		"events": []string{events.TypeBackupFailed}, // not the type published below
	}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating: status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	dispatcher := events.NewDispatcher(WebhookSource(env.db), env.db.Webhooks, silentLogger())
	dispatcher.Client = receiver.Client()
	dispatcher.AllowPrivateHosts = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx, env.api.Events())
	time.Sleep(100 * time.Millisecond)

	env.api.Events().Publish(events.Event{Type: events.TypeServerCrashed, OwnerID: env.user.ID})
	time.Sleep(500 * time.Millisecond)

	if got := atomic.LoadInt32(&deliveries); got != 0 {
		t.Fatalf("a hook received %d deliveries of a type it did not subscribe to", got)
	}
}

// --- console parsing ---

func TestParsePlayerLine(t *testing.T) {
	cases := []struct {
		line   string
		name   string
		joined bool
		ok     bool
	}{
		{"[12:34:56] [Server thread/INFO]: Steve joined the game", "Steve", true, true},
		{"[12:34:56] [Server thread/INFO]: Steve left the game", "Steve", false, true},
		{"[12:34:56 INFO]: Notch_2 joined the game", "Notch_2", true, true},
		{"[12:34:56] [Server thread/INFO]: A_very_long_name1 joined the game", "A_very_long_name1", false, false},
		// A player saying it in chat must not be mistaken for the real thing.
		{"[12:34:56] [Server thread/INFO]: <Steve> Herobrine joined the game", "", false, false},
		{"[12:34:56] [Server thread/INFO]: Steve lost connection: Disconnected", "", false, false},
		{"Steve joined the game", "", false, false},
		{"", "", false, false},
	}

	for _, tc := range cases {
		name, joined, ok := parsePlayerLine(tc.line)
		if ok != tc.ok {
			t.Errorf("parsePlayerLine(%q) ok = %v, want %v", tc.line, ok, tc.ok)
			continue
		}
		if ok && (name != tc.name || joined != tc.joined) {
			t.Errorf("parsePlayerLine(%q) = %q, %v; want %q, %v",
				tc.line, name, joined, tc.name, tc.joined)
		}
	}
}

// A crash has to raise both the status change and the crash event: a client
// watching only for crashes must not have to infer them from a status string.
func TestCrashEmitsBothEvents(t *testing.T) {
	env := newTestEnv(t)

	stream, unsubscribe := env.api.Events().Subscribe(t.Context(), env.user.ID, false)
	defer unsubscribe()

	env.api.emitStatus(testServerID, env.user.ID, runner.StatusCrashed)

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case event := <-stream:
			seen[event.Type] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only saw %v", seen)
		}
	}
	if !seen[events.TypeServerStatusChanged] || !seen[events.TypeServerCrashed] {
		t.Fatalf("saw %v", seen)
	}
}
