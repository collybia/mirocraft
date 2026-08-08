package runner

import (
	"context"
	"sync"
	"sync/atomic"
)

// subscriberQueue is the per-subscriber buffer. A subscriber that falls this
// far behind starts losing messages instead of stalling the producer.
const subscriberQueue = 256

// bus fans a value out to every active subscriber without ever blocking the
// producer: a subscriber whose queue is full drops the message. This is the
// drop policy that keeps one slow console viewer from stalling the process
// reader and, through it, the server itself.
type bus[T any] struct {
	mu     sync.RWMutex
	subs   map[*subscription[T]]struct{}
	closed bool
}

type subscription[T any] struct {
	ch      chan T
	dropped atomic.Uint64
	once    sync.Once
}

func newBus[T any]() *bus[T] {
	return &bus[T]{subs: make(map[*subscription[T]]struct{})}
}

// subscribe registers a new subscriber and returns its channel plus an
// idempotent unsubscribe function. The subscription is also released when ctx
// is cancelled, so callers that already have a request context cannot leak it.
//
// The returned channel is closed on unsubscribe and when the bus itself is
// closed, so a range loop over it terminates on server or daemon shutdown.
func (b *bus[T]) subscribe(ctx context.Context) (<-chan T, func()) {
	sub := &subscription[T]{ch: make(chan T, subscriberQueue)}

	b.mu.Lock()
	if b.closed {
		// Already closed: hand back a closed channel so the caller's read loop
		// terminates immediately instead of blocking forever.
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

func (b *bus[T]) remove(sub *subscription[T]) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subs[sub]; !ok {
		return
	}
	delete(b.subs, sub)
	sub.once.Do(func() { close(sub.ch) })
}

// publish delivers v to every subscriber, skipping those whose queue is full.
//
// The read lock is held across the sends, which is safe precisely because the
// sends are non-blocking: close() takes the write lock, so a channel can never
// be closed while a send on it is in flight.
func (b *bus[T]) publish(v T) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}
	for sub := range b.subs {
		select {
		case sub.ch <- v:
		default:
			sub.dropped.Add(1)
		}
	}
}

// close releases every subscription and rejects further publishes. It is
// idempotent, so stopping a server and shutting the daemon down cannot
// double-close a subscriber channel.
func (b *bus[T]) close() {
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

func (b *bus[T]) subscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
