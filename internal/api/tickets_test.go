package api

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fixedClock lets the TTL be tested without sleeping.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestTicketStore(ttl time.Duration) (*TicketStore, *fixedClock) {
	clock := &fixedClock{t: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
	s := NewTicketStore(ttl)
	s.now = clock.now
	return s, clock
}

func TestTicketIssueAndRedeem(t *testing.T) {
	s, _ := newTestTicketStore(TicketTTL)

	token, expiresAt, err := s.Issue("srv-1", "user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token == "" {
		t.Fatal("Issue returned an empty ticket")
	}
	if expiresAt.IsZero() {
		t.Fatal("Issue returned a zero expiry")
	}

	got, err := s.Redeem(token)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.ServerID != "srv-1" || got.UserID != "user-1" {
		t.Fatalf("Redeem returned %+v, want server srv-1 / user user-1", got)
	}
}

// A ticket must work exactly once, so a leaked console URL cannot be replayed.
func TestTicketIsSingleUse(t *testing.T) {
	s, _ := newTestTicketStore(TicketTTL)

	token, _, err := s.Issue("srv-1", "user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := s.Redeem(token); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := s.Redeem(token); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("second Redeem = %v, want ErrTicketInvalid", err)
	}
	if n := s.Len(); n != 0 {
		t.Fatalf("Len() = %d after redeem, want 0", n)
	}
}

func TestTicketExpires(t *testing.T) {
	const ttl = 30 * time.Second
	s, clock := newTestTicketStore(ttl)

	token, _, err := s.Issue("srv-1", "user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Still valid a moment before expiry.
	clock.advance(ttl - time.Millisecond)
	if _, err := s.Redeem(token); err != nil {
		t.Fatalf("Redeem just before expiry = %v, want success", err)
	}

	token2, _, err := s.Issue("srv-1", "user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	clock.advance(ttl)
	if _, err := s.Redeem(token2); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("Redeem at expiry = %v, want ErrTicketInvalid", err)
	}
}

func TestTicketUnknownTokenRejected(t *testing.T) {
	s, _ := newTestTicketStore(TicketTTL)

	if _, err := s.Redeem("not-a-ticket"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("Redeem of unknown token = %v, want ErrTicketInvalid", err)
	}
	if _, err := s.Redeem(""); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("Redeem of empty token = %v, want ErrTicketInvalid", err)
	}
}

// Expired tickets must not pile up in memory.
func TestTicketExpiredEntriesAreSwept(t *testing.T) {
	const ttl = 30 * time.Second
	s, clock := newTestTicketStore(ttl)

	for i := 0; i < 10; i++ {
		if _, _, err := s.Issue("srv-1", "user-1"); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	if n := s.Len(); n != 10 {
		t.Fatalf("Len() = %d, want 10", n)
	}

	clock.advance(ttl + time.Second)
	if _, _, err := s.Issue("srv-1", "user-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if n := s.Len(); n != 1 {
		t.Fatalf("Len() = %d after sweep, want 1 (only the fresh ticket)", n)
	}
}

func TestTicketsAreUnique(t *testing.T) {
	s, _ := newTestTicketStore(TicketTTL)

	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		token, _, err := s.Issue("srv-1", "user-1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if _, dup := seen[token]; dup {
			t.Fatalf("Issue returned a duplicate ticket after %d issues", i)
		}
		seen[token] = struct{}{}
	}
}

func TestTicketStoreConcurrentUse(t *testing.T) {
	s := NewTicketStore(TicketTTL)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				token, _, err := s.Issue("srv-1", "user-1")
				if err != nil {
					t.Errorf("Issue: %v", err)
					return
				}
				if _, err := s.Redeem(token); err != nil {
					t.Errorf("Redeem: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := s.Len(); n != 0 {
		t.Fatalf("Len() = %d after every ticket was redeemed, want 0", n)
	}
}

func TestHashTokenIsStableAndDistinct(t *testing.T) {
	if HashToken("abc") != HashToken("abc") {
		t.Fatal("HashToken is not deterministic")
	}
	if HashToken("abc") == HashToken("abd") {
		t.Fatal("HashToken collided on different inputs")
	}
	if HashToken("abc") == "abc" {
		t.Fatal("HashToken returned the raw token")
	}
}
