package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- the bus ---

func TestBusDeliversToTheOwner(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	stream, unsubscribe := bus.Subscribe(context.Background(), "user-1", false)
	defer unsubscribe()

	bus.Publish(Event{Type: TypeServerStatusChanged, OwnerID: "user-1", ServerID: "s1"})

	select {
	case event := <-stream:
		if event.Type != TypeServerStatusChanged || event.ServerID != "s1" {
			t.Fatalf("event = %+v", event)
		}
		if event.At.IsZero() {
			t.Error("the event has no timestamp")
		}
	case <-time.After(time.Second):
		t.Fatal("the event never arrived")
	}
}

// One user's server events must not reach another user's socket.
func TestBusDoesNotLeakBetweenUsers(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	mine, stopMine := bus.Subscribe(context.Background(), "user-1", false)
	defer stopMine()
	theirs, stopTheirs := bus.Subscribe(context.Background(), "user-2", false)
	defer stopTheirs()

	bus.Publish(Event{Type: TypeServerCrashed, OwnerID: "user-1", ServerID: "s1"})

	select {
	case <-mine:
	case <-time.After(time.Second):
		t.Fatal("the owner did not receive their own event")
	}

	select {
	case event := <-theirs:
		t.Fatalf("another user received %+v", event)
	case <-time.After(200 * time.Millisecond):
	}
}

// An admin subscription sees everything, which is what the webhook dispatcher
// relies on to route events to their owners.
func TestBusAdminSeesEverything(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	stream, unsubscribe := bus.Subscribe(context.Background(), "", true)
	defer unsubscribe()

	bus.Publish(Event{Type: TypeServerCrashed, OwnerID: "somebody-else"})

	select {
	case <-stream:
	case <-time.After(time.Second):
		t.Fatal("an admin subscription missed another user's event")
	}
}

// A stuck browser tab must not hold up the runner that produced the event.
func TestPublishNeverBlocksOnASlowSubscriber(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	_, stopSlow := bus.Subscribe(context.Background(), "user-1", false) // never read
	defer stopSlow()

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize*4; i++ {
			bus.Publish(Event{Type: TypeTaskUpdated, OwnerID: "user-1"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	stream, unsubscribe := bus.Subscribe(context.Background(), "user-1", false)
	unsubscribe()
	unsubscribe()

	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("the channel yielded a value after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel was not closed")
	}
	if bus.SubscriberCount() != 0 {
		t.Fatalf("SubscriberCount = %d after unsubscribe", bus.SubscriberCount())
	}
}

func TestCloseReleasesSubscribers(t *testing.T) {
	bus := NewBus()
	stream, _ := bus.Subscribe(context.Background(), "user-1", false)

	bus.Close()
	bus.Close() // idempotent

	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("the channel yielded a value after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release the subscription")
	}
}

func TestIsKnownType(t *testing.T) {
	for _, known := range AllTypes {
		if !IsKnownType(known) {
			t.Errorf("IsKnownType(%q) = false", known)
		}
	}
	// Subscribing to a typo would silently deliver nothing.
	for _, unknown := range []string{"", "server.status", "player.join", "nonsense"} {
		if IsKnownType(unknown) {
			t.Errorf("IsKnownType(%q) = true", unknown)
		}
	}
}

func TestBusConcurrentUse(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			bus.Publish(Event{Type: TypeTaskUpdated, OwnerID: "user-1"})
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ctx, cancel := context.WithCancel(context.Background())
				stream, unsubscribe := bus.Subscribe(ctx, "user-1", false)
				select {
				case <-stream:
				default:
				}
				unsubscribe()
				cancel()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- signing ---

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"type":"server.crashed"}`)
	signature := Sign("whsec_secret", body)

	if !Verify("whsec_secret", body, signature) {
		t.Fatal("a signature did not verify against its own body")
	}
	if Verify("another secret", body, signature) {
		t.Fatal("a signature verified under the wrong secret")
	}
	if Verify("whsec_secret", []byte(`{"type":"tampered"}`), signature) {
		t.Fatal("a signature verified against a tampered body")
	}
	if len(signature) < 20 || signature[:7] != "sha256=" {
		t.Fatalf("signature = %q, want the sha256= form", signature)
	}
}

// --- delivery ---

type stubSource struct{ targets []Target }

func (s stubSource) TargetsFor(context.Context, Event) ([]Target, error) {
	return s.targets, nil
}

type stubRecorder struct {
	mu      sync.Mutex
	records []struct {
		status int
		err    string
	}
}

func (r *stubRecorder) RecordDelivery(_ context.Context, _ string, status int, deliveryErr string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, struct {
		status int
		err    string
	}{status, deliveryErr})
	return nil
}

func (r *stubRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func (r *stubRecorder) last() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		return 0, ""
	}
	last := r.records[len(r.records)-1]
	return last.status, last.err
}

func TestDeliverSignsTheBody(t *testing.T) {
	const secret = "whsec_test_secret"

	var (
		gotSignature string
		gotEvent     string
		gotBody      []byte
		received     = make(chan struct{})
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get(SignatureHeader)
		gotEvent = r.Header.Get(EventHeader)
		gotBody, _ = io.ReadAll(r.Body)
		close(received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	recorder := &stubRecorder{}
	d := NewDispatcher(stubSource{}, recorder, discardLogger())
	d.Client = server.Client()
	d.AllowPrivateHosts = true // httptest listens on loopback

	d.deliver(context.Background(),
		Target{ID: "h1", URL: server.URL, Secret: secret},
		Event{Type: TypeServerCrashed, ServerID: "s1", At: time.Now()})

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never arrived")
	}

	if !Verify(secret, gotBody, gotSignature) {
		t.Fatalf("the signature does not verify: %q", gotSignature)
	}
	if gotEvent != TypeServerCrashed {
		t.Errorf("event header = %q", gotEvent)
	}

	var event Event
	if err := json.Unmarshal(gotBody, &event); err != nil {
		t.Fatalf("the body is not the event: %v", err)
	}
	if event.Type != TypeServerCrashed || event.ServerID != "s1" {
		t.Errorf("delivered event = %+v", event)
	}
	// The owner is routing information, not the receiver's business.
	if strings.Contains(string(gotBody), "owner") {
		t.Errorf("the delivery carries the owner id: %s", gotBody)
	}
}

// A receiver that is briefly down must not lose the event.
func TestDeliveryRetriesOnServerError(t *testing.T) {
	var attempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	recorder := &stubRecorder{}
	d := NewDispatcher(stubSource{}, recorder, discardLogger())
	d.Client = server.Client()
	d.AllowPrivateHosts = true

	// The real backoff would make this test take six seconds.
	defer func(original time.Duration) { RetryBase = original }(RetryBase)
	RetryBase = 5 * time.Millisecond

	start := time.Now()
	d.deliver(context.Background(),
		Target{ID: "h1", URL: server.URL, Secret: "secret"},
		Event{Type: TypeTaskUpdated, At: time.Now()})

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("the endpoint saw %d attempts, want 3", got)
	}
	if status, err := recorder.last(); status != http.StatusOK || err != "" {
		t.Fatalf("recorded %d %q, want a success", status, err)
	}
	t.Logf("three attempts took %s", time.Since(start).Round(time.Millisecond))
}

// A receiver that understood and refused will refuse again, so repeating the
// request only wastes both sides' time.
func TestDeliveryDoesNotRetryClientErrors(t *testing.T) {
	var attempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	d := NewDispatcher(stubSource{}, &stubRecorder{}, discardLogger())
	d.Client = server.Client()
	d.AllowPrivateHosts = true

	d.deliver(context.Background(),
		Target{ID: "h1", URL: server.URL, Secret: "secret"},
		Event{Type: TypeTaskUpdated, At: time.Now()})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("a 400 was retried %d times", got)
	}
}

// A rate-limited receiver is asking for a pause, not refusing outright.
func TestDeliveryRetriesRateLimits(t *testing.T) {
	var attempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	defer func(original time.Duration) { RetryBase = original }(RetryBase)
	RetryBase = 5 * time.Millisecond

	d := NewDispatcher(stubSource{}, &stubRecorder{}, discardLogger())
	d.Client = server.Client()
	d.AllowPrivateHosts = true

	d.deliver(context.Background(),
		Target{ID: "h1", URL: server.URL, Secret: "secret"},
		Event{Type: TypeTaskUpdated, At: time.Now()})

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("a 429 produced %d attempts, want 2", got)
	}
}

// A webhook URL is user-supplied and fetched by the daemon, which is exactly
// the shape of a server-side request forgery.
func TestDeliveryRefusesPrivateAddressesByDefault(t *testing.T) {
	var reached int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&reached, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	recorder := &stubRecorder{}
	d := NewDispatcher(stubSource{}, recorder, discardLogger())
	d.Client = server.Client()
	// AllowPrivateHosts is deliberately left off; httptest is on loopback.

	d.deliver(context.Background(),
		Target{ID: "h1", URL: server.URL, Secret: "secret"},
		Event{Type: TypeTaskUpdated, At: time.Now()})

	if atomic.LoadInt32(&reached) != 0 {
		t.Fatal("a delivery reached a loopback address with private hosts disabled")
	}
	if _, err := recorder.last(); err == "" {
		t.Fatal("the refusal was not recorded")
	}
}

func TestDeliveryRefusesNonHTTPSchemes(t *testing.T) {
	recorder := &stubRecorder{}
	d := NewDispatcher(stubSource{}, recorder, discardLogger())

	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com", "gopher://x", "not a url at all"} {
		recorder.records = nil
		d.deliver(context.Background(),
			Target{ID: "h1", URL: raw, Secret: "secret"},
			Event{Type: TypeTaskUpdated, At: time.Now()})

		if recorder.count() == 0 {
			t.Errorf("the refusal of %q was not recorded", raw)
		}
	}
}

// The dispatcher must deliver what the bus carries, end to end.
func TestDispatcherRunDeliversFromTheBus(t *testing.T) {
	received := make(chan string, 4)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event Event
		_ = json.Unmarshal(body, &event)
		received <- event.Type
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bus := NewBus()
	defer bus.Close()

	d := NewDispatcher(
		stubSource{targets: []Target{{ID: "h1", URL: server.URL, Secret: "secret"}}},
		&stubRecorder{}, discardLogger())
	d.Client = server.Client()
	d.AllowPrivateHosts = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, bus)

	// The dispatcher has to be subscribed before the event is published.
	time.Sleep(100 * time.Millisecond)
	bus.Publish(Event{Type: TypeBackupCompleted, OwnerID: "user-1"})

	select {
	case got := <-received:
		if got != TypeBackupCompleted {
			t.Fatalf("delivered %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatcher delivered nothing")
	}
}
