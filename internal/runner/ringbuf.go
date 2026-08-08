package runner

import (
	"sync"
	"time"
)

// Stream names for ConsoleLine.Stream.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// ConsoleLine is a single line captured from a server process.
type ConsoleLine struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

// RingBuffer is a fixed-capacity, concurrency-safe buffer of console lines.
// Once full it overwrites the oldest entry, so it always holds the most recent
// Cap() lines. All methods are safe for concurrent use.
type RingBuffer struct {
	mu    sync.RWMutex
	buf   []ConsoleLine
	next  int // index the next write goes to
	count int // number of valid entries, never exceeds len(buf)
}

// NewRingBuffer returns a buffer holding at most capacity lines.
// It panics on a non-positive capacity, which is always a programming error.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		panic("runner: ring buffer capacity must be positive")
	}
	return &RingBuffer{buf: make([]ConsoleLine, capacity)}
}

// Add appends a line, evicting the oldest one when the buffer is full.
func (r *RingBuffer) Add(line ConsoleLine) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buf[r.next] = line
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// Last returns up to n most recent lines in chronological order (oldest first).
// The result is a copy and never nil, so callers may retain or marshal it freely.
func (r *RingBuffer) Last(n int) []ConsoleLine {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n > r.count {
		n = r.count
	}
	if n <= 0 {
		return []ConsoleLine{}
	}

	out := make([]ConsoleLine, n)
	start := (r.next - n + len(r.buf)) % len(r.buf)
	for i := range out {
		out[i] = r.buf[(start+i)%len(r.buf)]
	}
	return out
}

// Len reports how many lines are currently buffered.
func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// Cap reports the buffer capacity.
func (r *RingBuffer) Cap() int {
	return len(r.buf)
}
