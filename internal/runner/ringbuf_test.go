package runner

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func line(text string) ConsoleLine {
	return ConsoleLine{TS: time.Now().UTC(), Stream: StreamStdout, Text: text}
}

func texts(lines []ConsoleLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRingBufferEmpty(t *testing.T) {
	r := NewRingBuffer(4)

	if got := r.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if got := r.Cap(); got != 4 {
		t.Fatalf("Cap() = %d, want 4", got)
	}
	got := r.Last(10)
	if got == nil {
		t.Fatal("Last() returned nil, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("Last() on empty buffer = %v, want empty", texts(got))
	}
}

func TestRingBufferBelowCapacity(t *testing.T) {
	r := NewRingBuffer(4)
	r.Add(line("a"))
	r.Add(line("b"))

	if got := r.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
	if got := texts(r.Last(10)); !equalStrings(got, []string{"a", "b"}) {
		t.Fatalf("Last(10) = %v, want [a b]", got)
	}
	if got := texts(r.Last(1)); !equalStrings(got, []string{"b"}) {
		t.Fatalf("Last(1) = %v, want [b] (most recent)", got)
	}
}

func TestRingBufferOverflowEvictsOldest(t *testing.T) {
	r := NewRingBuffer(3)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		r.Add(line(s))
	}

	if got := r.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3 (capacity)", got)
	}
	if got := texts(r.Last(10)); !equalStrings(got, []string{"c", "d", "e"}) {
		t.Fatalf("Last(10) after overflow = %v, want [c d e]", got)
	}
	if got := texts(r.Last(2)); !equalStrings(got, []string{"d", "e"}) {
		t.Fatalf("Last(2) after overflow = %v, want [d e]", got)
	}
}

// Wrapping repeatedly must not corrupt ordering: the buffer is exercised well
// past a whole number of laps.
func TestRingBufferManyLaps(t *testing.T) {
	const capacity = 8
	r := NewRingBuffer(capacity)
	for i := 0; i < capacity*7+3; i++ {
		r.Add(line(fmt.Sprintf("%d", i)))
	}

	total := capacity*7 + 3
	want := make([]string, capacity)
	for i := range want {
		want[i] = fmt.Sprintf("%d", total-capacity+i)
	}
	if got := texts(r.Last(capacity)); !equalStrings(got, want) {
		t.Fatalf("Last(%d) = %v, want %v", capacity, got, want)
	}
}

func TestRingBufferLastNonPositive(t *testing.T) {
	r := NewRingBuffer(4)
	r.Add(line("a"))

	for _, n := range []int{0, -1, -100} {
		if got := r.Last(n); len(got) != 0 {
			t.Fatalf("Last(%d) = %v, want empty", n, texts(got))
		}
	}
}

func TestNewRingBufferPanicsOnNonPositiveCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewRingBuffer(%d) did not panic", capacity)
				}
			}()
			NewRingBuffer(capacity)
		}()
	}
}

// Run with -race: concurrent writers and readers must not race, and the
// buffer must stay internally consistent.
func TestRingBufferConcurrentAccess(t *testing.T) {
	const (
		capacity = 64
		writers  = 8
		readers  = 4
		perWrite = 500
	)

	r := NewRingBuffer(capacity)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWrite; i++ {
				r.Add(line(fmt.Sprintf("w%d-%d", w, i)))
			}
		}(w)
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := r.Last(capacity)
				if len(got) > capacity {
					t.Errorf("Last() returned %d lines, exceeds capacity %d", len(got), capacity)
					return
				}
				if n := r.Len(); n > capacity {
					t.Errorf("Len() = %d, exceeds capacity %d", n, capacity)
					return
				}
			}
		}()
	}

	// Writers finish first, then readers are told to stop.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()

	wg.Wait()

	if got := r.Len(); got != capacity {
		t.Fatalf("Len() = %d after %d writes, want %d", got, writers*perWrite, capacity)
	}
}
