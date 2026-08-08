package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want error
	}{
		{"simple", "list", nil},
		{"with args", "say hello world", nil},
		{"cyrillic", "say Рестарт через 5 минут", nil},
		{"leading slash", "/give Steve dirt 1", nil},
		{"at max length", strings.Repeat("a", MaxCommandRunes), nil},
		{"cyrillic at max length", strings.Repeat("я", MaxCommandRunes), nil},

		{"empty", "", ErrCommandEmpty},
		{"only spaces", "   ", ErrCommandEmpty},
		{"only tab is control", "\t", ErrCommandControl},
		{"over max length", strings.Repeat("a", MaxCommandRunes+1), ErrCommandTooLong},
		{"carriage return", "say hi\rop Steve", ErrCommandControl},
		{"newline injects a second command", "say hi\nop Steve", ErrCommandControl},
		{"embedded tab", "say a\tb", ErrCommandControl},
		{"null byte", "say hi\x00", ErrCommandControl},
		{"escape sequence", "say \x1b[31mred", ErrCommandControl},
		{"delete char", "say hi\x7f", ErrCommandControl},
		{"invalid utf8", "say \xff\xfe", ErrCommandNotUTF8},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCommand(tc.cmd)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateCommand(%q) = %v, want %v", tc.cmd, err, tc.want)
			}
		})
	}
}

// A cyrillic command at the rune limit is well over 512 bytes; the limit must
// be counted in runes, not bytes.
func TestValidateCommandCountsRunesNotBytes(t *testing.T) {
	cmd := strings.Repeat("я", MaxCommandRunes)
	if len(cmd) <= MaxCommandRunes {
		t.Fatalf("test precondition: %d bytes should exceed %d", len(cmd), MaxCommandRunes)
	}
	if err := ValidateCommand(cmd); err != nil {
		t.Fatalf("ValidateCommand(%d runes / %d bytes) = %v, want nil", MaxCommandRunes, len(cmd), err)
	}
}

func TestHubHistory(t *testing.T) {
	h := NewHub(3)
	for _, s := range []string{"a", "b", "c", "d"} {
		h.Publish(line(s))
	}

	if got := texts(h.History(10)); !equalStrings(got, []string{"b", "c", "d"}) {
		t.Fatalf("History(10) = %v, want [b c d]", got)
	}
	if got := texts(h.History(2)); !equalStrings(got, []string{"c", "d"}) {
		t.Fatalf("History(2) = %v, want [c d]", got)
	}
}

func TestHubSubscribeReceivesLines(t *testing.T) {
	h := NewHub(10)
	ch, unsubscribe := h.Subscribe(context.Background())
	defer unsubscribe()

	h.Publish(line("hello"))

	select {
	case got := <-ch:
		if got.Text != "hello" {
			t.Fatalf("received %q, want %q", got.Text, "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a published line")
	}
}

func TestHubFanOutToAllSubscribers(t *testing.T) {
	h := NewHub(10)

	const n = 5
	chans := make([]<-chan ConsoleLine, n)
	for i := range chans {
		ch, unsubscribe := h.Subscribe(context.Background())
		defer unsubscribe()
		chans[i] = ch
	}

	if got := h.SubscriberCount(); got != n {
		t.Fatalf("SubscriberCount() = %d, want %d", got, n)
	}

	h.Publish(line("broadcast"))

	for i, ch := range chans {
		select {
		case got := <-ch:
			if got.Text != "broadcast" {
				t.Fatalf("subscriber %d received %q", i, got.Text)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive the line", i)
		}
	}
}

func TestHubUnsubscribeClosesChannelAndIsIdempotent(t *testing.T) {
	h := NewHub(10)
	ch, unsubscribe := h.Subscribe(context.Background())

	unsubscribe()
	unsubscribe() // must not panic on a double close

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel yielded a value after unsubscribe, want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed by unsubscribe")
	}

	if got := h.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount() = %d after unsubscribe, want 0", got)
	}
}

func TestHubContextCancellationUnsubscribes(t *testing.T) {
	h := NewHub(10)
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := h.Subscribe(ctx)

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel yielded a value after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not release the subscription")
	}
}

func TestHubCloseReleasesSubscribers(t *testing.T) {
	h := NewHub(10)
	ch, _ := h.Subscribe(context.Background())

	h.Close()
	h.Close() // idempotent: server stop then daemon shutdown

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel yielded a value after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release the subscription")
	}
}

func TestHubSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	h := NewHub(10)
	h.Close()

	ch, unsubscribe := h.Subscribe(context.Background())
	defer unsubscribe()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("subscribing to a closed hub yielded a value")
		}
	case <-time.After(time.Second):
		t.Fatal("subscribing to a closed hub returned a channel that never closes")
	}
}

// The drop policy is the point of the design: a subscriber that never reads
// must not stall Publish, and the other subscribers must keep receiving.
func TestHubSlowSubscriberDropsWithoutBlockingOthers(t *testing.T) {
	h := NewHub(10)

	_, stopSlow := h.Subscribe(context.Background()) // never read from
	defer stopSlow()

	fastCh, stopFast := h.Subscribe(context.Background())
	defer stopFast()

	const total = subscriberQueue * 4
	drained := make(chan int, 1)
	go func() {
		count := 0
		for range fastCh {
			count++
			if count == total {
				break
			}
		}
		drained <- count
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			h.Publish(line("x"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads — drop policy is not working")
	}

	select {
	case count := <-drained:
		if count != total {
			t.Fatalf("fast subscriber received %d lines, want %d", count, total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast subscriber did not receive every line while a slow one was stuck")
	}
}

func TestHubStatusSubscription(t *testing.T) {
	h := NewHub(10)
	ch, unsubscribe := h.SubscribeStatus(context.Background())
	defer unsubscribe()

	h.PublishStatus(StatusRunning)

	select {
	case got := <-ch:
		if got != StatusRunning {
			t.Fatalf("received status %q, want %q", got, StatusRunning)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a status change")
	}
}

// Publishing while subscribers come and go must not race or send on a closed
// channel. Meaningful under -race.
func TestHubConcurrentSubscribeAndPublish(t *testing.T) {
	h := NewHub(100)
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
			h.Publish(line("tick"))
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ctx, cancel := context.WithCancel(context.Background())
				ch, unsubscribe := h.Subscribe(ctx)
				select {
				case <-ch:
				default:
				}
				if j%2 == 0 {
					unsubscribe()
				} else {
					cancel()
				}
				cancel()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	h.Close()
}
