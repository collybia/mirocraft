// Package events carries what happens on a server to whoever is watching:
// the panel over a WebSocket, and webhooks over HTTP.
//
// Both readers want the same facts, so there is one bus and two consumers,
// rather than each feature growing its own notification path.
package events

import (
	"context"
	"sync"
	"time"
)

// Event types, as documented in docs/API.md.
const (
	TypeServerStatusChanged = "server.status_changed"
	TypeServerCrashed       = "server.crashed"
	TypeBackupCompleted     = "backup.completed"
	TypeBackupFailed        = "backup.failed"
	TypePlayerJoined        = "player.joined"
	TypePlayerLeft          = "player.left"
	TypeTaskUpdated         = "task.updated"
)

// AllTypes is every event a client may subscribe to.
var AllTypes = []string{
	TypeServerStatusChanged,
	TypeServerCrashed,
	TypeBackupCompleted,
	TypeBackupFailed,
	TypePlayerJoined,
	TypePlayerLeft,
	TypeTaskUpdated,
}

// IsKnownType reports whether a type is one the panel emits.
//
// Subscribing to a typo would silently deliver nothing, so the API rejects
// unknown types rather than accepting them.
func IsKnownType(t string) bool {
	for _, known := range AllTypes {
		if known == t {
			return true
		}
	}
	return false
}

// Event is one thing that happened.
type Event struct {
	Type     string         `json:"type"`
	ServerID string         `json:"server_id,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	At       time.Time      `json:"at"`

	// OwnerID routes the event to the user who owns the server. It is not
	// serialised: a subscriber already knows who they are, and telling one
	// user another's id would be a small leak for no purpose.
	OwnerID string `json:"-"`
}

// queueSize is the per-subscriber buffer. A subscriber that falls this far
// behind starts losing events rather than stalling the publisher.
const queueSize = 128

// Bus fans events out to subscribers.
//
// Publishing never blocks. The alternative — a slow WebSocket viewer holding
// up the runner that produced the event — is how one stuck browser tab takes
// a server down with it.
type Bus struct {
	mu     sync.RWMutex
	subs   map[*subscription]struct{}
	closed bool
}

type subscription struct {
	ch      chan Event
	ownerID string
	all     bool // an admin sees every server
	once    sync.Once
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[*subscription]struct{})}
}

// Subscribe returns a channel of events for a user, plus an idempotent
// unsubscribe. An admin subscription receives events for every server.
//
// The channel closes on unsubscribe, on ctx cancellation and when the bus is
// closed, so a range loop over it terminates on shutdown.
func (b *Bus) Subscribe(ctx context.Context, ownerID string, all bool) (<-chan Event, func()) {
	sub := &subscription{ch: make(chan Event, queueSize), ownerID: ownerID, all: all}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(sub.ch)
		return sub.ch, func() {}
	}
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() { b.remove(sub) }

	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			unsubscribe()
		}()
	}
	return sub.ch, unsubscribe
}

func (b *Bus) remove(sub *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subs[sub]; !ok {
		return
	}
	delete(b.subs, sub)
	sub.once.Do(func() { close(sub.ch) })
}

// Publish delivers an event to everyone entitled to see it.
func (b *Bus) Publish(event Event) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}
	for sub := range b.subs {
		if !sub.all && sub.ownerID != event.OwnerID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// Dropped rather than blocking. The console has its own history
			// for catching up; events are notifications, not a ledger.
		}
	}
}

// Close releases every subscription. It is idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true
	for sub := range b.subs {
		sub.once.Do(func() { close(sub.ch) })
		delete(b.subs, sub)
	}
}

// SubscriberCount reports active subscriptions, for tests and metrics.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
