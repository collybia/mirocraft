package dns

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// DefaultCheckInterval is how often the public address is re-checked.
//
// Five minutes because that is the shape of the problem: a home connection
// changes address at most a few times a day, usually on reconnect, and the
// cost of noticing late is players hitting a stale record for a few minutes.
// Checking every thirty seconds would multiply the load on free services that
// ask politely not to be hammered, for no benefit anyone would notice.
const DefaultCheckInterval = 5 * time.Minute

// Watcher keeps a provider's records pointing at the current public address.
//
// The "mini-DynDNS" the architecture calls for: a home server or a VPS with a
// dynamic address would otherwise publish a name that stops resolving the
// first time the connection drops, and the operator would have no way to see
// why from inside the panel.
type Watcher struct {
	Provider Provider
	// Sub is the name under the zone to keep updated. Empty is the zone.
	Sub string
	// Interval overrides DefaultCheckInterval.
	Interval time.Duration
	// HTTP is used for the address lookup; nil builds one.
	HTTP *http.Client
	// Lookup overrides how the public address is found, for tests.
	Lookup func(ctx context.Context) (netip.Addr, error)

	log *slog.Logger

	mu      sync.Mutex
	current netip.Addr
	lastErr error
	checked time.Time
}

// NewWatcher returns a watcher for one name.
func NewWatcher(provider Provider, sub string, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{Provider: provider, Sub: sub, log: log}
}

// Status is what the panel shows about the published name.
type Status struct {
	// Name is the full hostname being kept up to date.
	Name string `json:"name"`
	// Address is what was last published. Invalid means nothing yet.
	Address string `json:"address,omitempty"`
	// CheckedAt is when the address was last looked up.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// Error is the last failure, if the name may be stale. Reported rather
	// than only logged: a name that stopped updating looks exactly like a
	// server that is down, and the operator should be able to tell them apart
	// from the panel.
	Error string `json:"error,omitempty"`
}

// Status reports the current state.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()

	status := Status{Name: FQDN(w.Sub, w.Provider.Zone()), CheckedAt: w.checked}
	if w.current.IsValid() {
		status.Address = w.current.String()
	}
	if w.lastErr != nil {
		status.Error = w.lastErr.Error()
	}
	return status
}

// Run keeps the record current until ctx is cancelled.
//
// Checks immediately rather than after the first interval: a daemon that has
// just started on a new address should publish it now, not in five minutes.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}

	if err := w.Check(ctx); err != nil {
		w.log.Warn("publishing the initial address failed", slog.String("error", err.Error()))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Check(ctx); err != nil {
				w.log.Warn("updating the address record failed", slog.String("error", err.Error()))
			}
		}
	}
}

// Check looks up the public address and publishes it if it moved.
func (w *Watcher) Check(ctx context.Context) error {
	lookup := w.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context) (netip.Addr, error) { return PublicIP(ctx, w.HTTP) }
	}

	addr, err := lookup(ctx)
	// A disagreement between sources still yields a usable address, so it is
	// logged and used rather than treated as a failure.
	if err != nil && addr.IsValid() {
		w.log.Warn("public address lookup was not unanimous", slog.String("error", err.Error()))
		err = nil
	}
	if err != nil {
		w.record(netip.Addr{}, err)
		return err
	}

	w.mu.Lock()
	unchanged := w.current.IsValid() && w.current == addr && w.lastErr == nil
	w.mu.Unlock()

	// An unchanged address is the overwhelmingly common case, and rewriting
	// the same record every five minutes is exactly the traffic these free
	// services ask people not to generate.
	if unchanged {
		w.record(addr, nil)
		return nil
	}

	if err := w.Provider.EnsureAddress(ctx, w.Sub, addr); err != nil {
		w.record(netip.Addr{}, err)
		return err
	}

	w.log.Info("address record published",
		slog.String("name", FQDN(w.Sub, w.Provider.Zone())), slog.String("address", addr.String()))
	w.record(addr, nil)
	return nil
}

// SRVPublisher is the part of a Provider that publishing one server's record
// needs.
//
// Narrower than Provider on purpose: the caller in the API holds only this,
// so nothing there can accidentally rewrite the panel's own address record
// while publishing a server's port.
type SRVPublisher interface {
	Zone() string
	Capabilities() Capabilities
	EnsureSRV(ctx context.Context, sub, target string, port int) error
}

// EnsureServerSRV publishes the SRV record for one Java server.
//
// Separate from the address watcher because the two change for different
// reasons: the address moves with the connection, the port moves when an
// operator edits the server. Called when a server is created or its port
// changes.
//
// A provider that cannot publish SRV is not an error here — the caller is told
// so it can explain that players will have to type the port.
func EnsureServerSRV(ctx context.Context, provider SRVPublisher, sub string, port int) error {
	if !provider.Capabilities().SRV {
		return ErrUnsupported
	}
	target := FQDN(sub, provider.Zone())
	return provider.EnsureSRV(ctx, sub, target, port)
}

func (w *Watcher) record(addr netip.Addr, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.checked = time.Now().UTC()
	w.lastErr = err
	if err == nil {
		w.current = addr
	}
}

// IsUnsupported reports whether an error is a provider limitation rather than
// a failure, so a caller can degrade instead of giving up.
func IsUnsupported(err error) bool { return errors.Is(err, ErrUnsupported) }
