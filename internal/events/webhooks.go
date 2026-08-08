package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Delivery settings.
const (
	// SignatureHeader carries the HMAC so a receiver can prove the delivery
	// came from this panel.
	SignatureHeader = "X-Mirocraft-Signature"
	// EventHeader names the event, so a receiver can route without parsing
	// the body first.
	EventHeader = "X-Mirocraft-Event"
	// DeliveryHeader is a unique id per delivery, so a receiver can drop a
	// duplicate that a retry produced.
	DeliveryHeader = "X-Mirocraft-Delivery"

	// Attempts is how many times one event is delivered before giving up.
	Attempts = 3
	// RequestTimeout bounds one attempt.
	RequestTimeout = 10 * time.Second
	// MaxResponseBytes is how much of a response is read, for the error
	// message only.
	MaxResponseBytes = 4 << 10
)

// RetryBase is the first backoff; each attempt doubles it. A variable rather
// than a constant so tests can exercise the retry path without waiting the six
// seconds a real backoff takes.
var RetryBase = 2 * time.Second

// Sign returns the signature header value for a body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature in constant time.
//
// Exported because it is the half a receiver has to implement, and having the
// reference here means the documentation can point at real code.
func Verify(secret string, body []byte, signature string) bool {
	expected := Sign(secret, body)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// Target is one webhook to deliver to.
type Target struct {
	ID     string
	URL    string
	Secret string
}

// Recorder stores the outcome of a delivery.
type Recorder interface {
	RecordDelivery(ctx context.Context, id string, status int, deliveryErr string, at time.Time) error
}

// TargetSource returns the hooks that want an event.
type TargetSource interface {
	TargetsFor(ctx context.Context, event Event) ([]Target, error)
}

// Dispatcher delivers events to webhooks.
type Dispatcher struct {
	Source   TargetSource
	Recorder Recorder
	Client   *http.Client

	// AllowPrivateHosts permits delivery to loopback and private addresses.
	// Off by default: see checkURL.
	AllowPrivateHosts bool

	log *slog.Logger
}

// NewDispatcher returns a dispatcher.
func NewDispatcher(source TargetSource, recorder Recorder, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		Source:   source,
		Recorder: recorder,
		Client:   &http.Client{Timeout: RequestTimeout},
		log:      log,
	}
}

// Run delivers every event on the bus until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context, bus *Bus) {
	// Subscribed as an admin: the dispatcher has to see every server's events
	// in order to route them to their owners' hooks.
	stream, unsubscribe := bus.Subscribe(ctx, "", true)
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-stream:
			if !ok {
				return
			}
			d.dispatch(ctx, event)
		}
	}
}

// dispatch sends one event to every hook that wants it.
func (d *Dispatcher) dispatch(ctx context.Context, event Event) {
	targets, err := d.Source.TargetsFor(ctx, event)
	if err != nil {
		d.log.Warn("reading webhook targets failed", slog.String("error", err.Error()))
		return
	}

	for _, target := range targets {
		// One slow endpoint must not hold up the next, and delivery outlives
		// the event, so each goes to its own goroutine with its own context.
		go d.deliver(context.WithoutCancel(ctx), target, event)
	}
}

// deliver posts one event, retrying on failure.
func (d *Dispatcher) deliver(ctx context.Context, target Target, event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		d.log.Warn("encoding an event failed", slog.String("error", err.Error()))
		return
	}

	if err := d.checkURL(target.URL); err != nil {
		d.record(ctx, target.ID, 0, err.Error())
		return
	}

	deliveryID := fmt.Sprintf("%s-%d", event.Type, event.At.UnixNano())
	signature := Sign(target.Secret, body)

	var lastStatus int
	var lastErr string

	for attempt := 1; attempt <= Attempts; attempt++ {
		status, err := d.attempt(ctx, target, body, signature, event.Type, deliveryID)
		lastStatus = status

		if err == nil {
			d.record(ctx, target.ID, status, "")
			return
		}
		lastErr = err.Error()

		// 4xx other than 429 means the receiver understood and refused, so
		// repeating the same request will not help.
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			break
		}
		if attempt == Attempts {
			break
		}

		backoff := RetryBase * time.Duration(1<<(attempt-1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}

	d.log.Warn("webhook delivery failed",
		slog.String("webhook_id", target.ID), slog.String("event", event.Type),
		slog.Int("status", lastStatus), slog.String("error", lastErr))
	d.record(ctx, target.ID, lastStatus, lastErr)
}

// attempt performs one delivery.
func (d *Dispatcher) attempt(ctx context.Context, target Target, body []byte, signature, eventType, deliveryID string) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mirocraft-Webhook/1.0")
	req.Header.Set(SignatureHeader, signature)
	req.Header.Set(EventHeader, eventType)
	req.Header.Set(DeliveryHeader, deliveryID)

	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
		return resp.StatusCode, nil
	}

	preview, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	return resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(preview)))
}

func (d *Dispatcher) record(ctx context.Context, id string, status int, deliveryErr string) {
	if d.Recorder == nil {
		return
	}
	if err := d.Recorder.RecordDelivery(ctx, id, status, deliveryErr, time.Now()); err != nil {
		d.log.Debug("recording a delivery failed", slog.String("error", err.Error()))
	}
}

// checkURL refuses a destination the panel should not be making requests to.
//
// A webhook URL is supplied by a user and fetched by the daemon, which is
// exactly the shape of a server-side request forgery: a hook pointing at
// 169.254.169.254 or at 127.0.0.1:8080 turns the panel into a proxy for its
// own network. Loopback and private ranges are refused unless an operator
// explicitly allows them, which they may reasonably want on a home server
// where the bot runs beside the panel.
func (d *Dispatcher) checkURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("the url is not valid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("the url must be http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("the url has no host")
	}
	if d.AllowPrivateHosts {
		return nil
	}

	host := parsed.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		// A name that does not resolve is not a security problem; the request
		// will simply fail and be recorded.
		return nil
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("refusing to deliver to the private address %s; "+
				"enable private webhook targets in the configuration if this is deliberate", ip)
		}
	}
	return nil
}
