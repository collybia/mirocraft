package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// TicketTTL is how long a console WebSocket ticket stays valid.
const TicketTTL = 30 * time.Second

// ErrTicketInvalid covers unknown, expired and already-redeemed tickets alike.
// They are deliberately indistinguishable to the caller.
var ErrTicketInvalid = errors.New("ticket is invalid, expired or already used")

// Ticket is a redeemed console ticket.
type Ticket struct {
	ServerID string
	UserID   string
}

type ticketEntry struct {
	Ticket
	expiresAt time.Time
}

// TicketStore issues single-use, short-lived tickets that authenticate a
// WebSocket upgrade.
//
// Tickets exist so the long-lived API token never appears in a URL, where it
// would end up in proxy logs and browser history.
//
// Storage is in memory: a ticket outlives neither its TTL nor a daemon
// restart, and there is nothing worth persisting.
type TicketStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time // injectable so tests need no sleeps
	tickets map[string]ticketEntry
}

// NewTicketStore returns a store issuing tickets valid for ttl.
// A non-positive ttl falls back to TicketTTL.
func NewTicketStore(ttl time.Duration) *TicketStore {
	if ttl <= 0 {
		ttl = TicketTTL
	}
	return &TicketStore{
		ttl:     ttl,
		now:     time.Now,
		tickets: make(map[string]ticketEntry),
	}
}

// Issue creates a ticket for the given server and user and returns its value
// and expiry.
func (s *TicketStore) Issue(serverID, userID string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.sweepLocked(now)

	expiresAt := now.Add(s.ttl)
	s.tickets[token] = ticketEntry{
		Ticket:    Ticket{ServerID: serverID, UserID: userID},
		expiresAt: expiresAt,
	}
	return token, expiresAt, nil
}

// Redeem consumes a ticket. A ticket is valid exactly once: a second attempt
// with the same value fails, so a leaked URL cannot be replayed.
func (s *TicketStore) Redeem(token string) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tickets[token]
	if !ok {
		return Ticket{}, ErrTicketInvalid
	}
	delete(s.tickets, token)

	if !s.now().Before(entry.expiresAt) {
		return Ticket{}, ErrTicketInvalid
	}
	return entry.Ticket, nil
}

// Len reports how many tickets are currently held. Used by tests and metrics.
func (s *TicketStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tickets)
}

// sweepLocked drops expired tickets. Called on Issue, which bounds the map by
// the issue rate and avoids a background goroutine for a map that is normally
// nearly empty.
func (s *TicketStore) sweepLocked(now time.Time) {
	for token, entry := range s.tickets {
		if !now.Before(entry.expiresAt) {
			delete(s.tickets, token)
		}
	}
}
