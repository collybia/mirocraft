package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// ConsoleBufferLines is how much scrollback a running server keeps in memory.
const ConsoleBufferLines = 1000

// MaxCommandRunes caps a single console command. Counted in runes, not bytes,
// so a Cyrillic `say` message is not cut short compared to a Latin one.
const MaxCommandRunes = 512

// Command validation errors. Callers map these onto validation_failed.
var (
	ErrCommandEmpty       = errors.New("command is empty")
	ErrCommandTooLong     = fmt.Errorf("command exceeds %d characters", MaxCommandRunes)
	ErrCommandControl     = errors.New("command contains control characters")
	ErrCommandNotUTF8     = errors.New("command is not valid UTF-8")
	ErrConsoleUnavailable = errors.New("console is not available: server is not running")
)

// ValidateCommand reports whether cmd may be written to a server's stdin.
//
// Control characters are rejected wholesale rather than stripped: a newline in
// the middle of a command would inject a second command, and silently rewriting
// what an operator typed is worse than refusing it.
func ValidateCommand(cmd string) error {
	if !utf8.ValidString(cmd) {
		return ErrCommandNotUTF8
	}
	// Control characters are checked before emptiness so that a command made
	// only of them reports the precise reason rather than "empty".
	for _, r := range cmd {
		if r < 0x20 || r == 0x7f {
			return ErrCommandControl
		}
	}
	if strings.TrimSpace(cmd) == "" {
		return ErrCommandEmpty
	}
	if utf8.RuneCountInString(cmd) > MaxCommandRunes {
		return ErrCommandTooLong
	}
	return nil
}

// Hub owns the console state of one running server: the scrollback buffer, the
// line fan-out and the status fan-out. It is safe for concurrent use.
type Hub struct {
	// mu orders buffer writes against subscription, so that
	// SubscribeWithHistory cannot miss a line that is published while it runs.
	mu       sync.Mutex
	buf      *RingBuffer
	lines    *bus[ConsoleLine]
	statuses *bus[Status]
}

// NewHub returns a hub with a scrollback buffer of the given capacity.
func NewHub(capacity int) *Hub {
	return &Hub{
		buf:      NewRingBuffer(capacity),
		lines:    newBus[ConsoleLine](),
		statuses: newBus[Status](),
	}
}

// Publish records a line in the scrollback and fans it out to subscribers.
// It never blocks: subscribers that cannot keep up lose lines.
func (h *Hub) Publish(line ConsoleLine) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.buf.Add(line)
	h.lines.publish(line)
}

// PublishStatus fans out a status change.
func (h *Hub) PublishStatus(s Status) {
	h.statuses.publish(s)
}

// History returns up to n most recent lines, oldest first.
func (h *Hub) History(n int) []ConsoleLine {
	return h.buf.Last(n)
}

// Subscribe returns a channel of new console lines and an idempotent
// unsubscribe function. The channel is closed on unsubscribe, on ctx
// cancellation and when the hub is closed.
//
// A subscriber that reads too slowly silently misses lines rather than
// stalling the producer — the buffered history is the way to catch up.
func (h *Hub) Subscribe(ctx context.Context) (<-chan ConsoleLine, func()) {
	return h.lines.subscribe(ctx)
}

// SubscribeWithHistory atomically snapshots up to n scrollback lines and
// subscribes to everything published afterwards.
//
// Doing both under one lock is what makes the WebSocket console gap-free:
// snapshotting and subscribing separately would drop any line published in
// between, which on a busy server is exactly the line an operator is watching
// for.
func (h *Hub) SubscribeWithHistory(ctx context.Context, n int) ([]ConsoleLine, <-chan ConsoleLine, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	history := h.buf.Last(n)
	ch, unsubscribe := h.lines.subscribe(ctx)
	return history, ch, unsubscribe
}

// SubscribeStatus returns a channel of status changes with the same semantics
// as Subscribe.
func (h *Hub) SubscribeStatus(ctx context.Context) (<-chan Status, func()) {
	return h.statuses.subscribe(ctx)
}

// Close releases every subscription. It is idempotent and is called when the
// server stops and when the daemon shuts down.
func (h *Hub) Close() {
	h.lines.close()
	h.statuses.close()
}

// SubscriberCount reports active line subscribers. Used by tests and metrics.
func (h *Hub) SubscriberCount() int {
	return h.lines.subscriberCount()
}
