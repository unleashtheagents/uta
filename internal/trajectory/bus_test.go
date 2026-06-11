package trajectory

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBus_PublishFansOutToAllSubscribers(t *testing.T) {
	b := NewBus()
	a := b.Subscribe(8)
	c := b.Subscribe(8)

	ev := Event{SessionID: "s", Seq: 1, Kind: GoalReceived}
	b.Publish(ev)

	for i, ch := range []<-chan Event{a, c} {
		select {
		case got := <-ch:
			if got.Seq != 1 || got.Kind != GoalReceived {
				t.Fatalf("subscriber %d: got %+v, want seq=1 kind=goal_received", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for event", i)
		}
	}
}

func TestBus_PublishWithNoSubscribers_NoPanic(t *testing.T) {
	b := NewBus()
	b.Publish(Event{Kind: GoalReceived})
	if got := b.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d, want 0", got)
	}
}

func TestBus_Subscribe_BufferDefaults(t *testing.T) {
	b := NewBus()
	ch := b.Subscribe(0)
	if cap(asChan(t, ch)) != 64 {
		t.Fatalf("Subscribe(0) cap = %d, want 64", cap(asChan(t, ch)))
	}
	ch = b.Subscribe(-5)
	if cap(asChan(t, ch)) != 64 {
		t.Fatalf("Subscribe(-5) cap = %d, want 64", cap(asChan(t, ch)))
	}
	ch = b.Subscribe(3)
	if cap(asChan(t, ch)) != 3 {
		t.Fatalf("Subscribe(3) cap = %d, want 3", cap(asChan(t, ch)))
	}
}

// asChan is a tiny helper that re-types a receive-only channel as a bidirectional one
// for cap() inspection in tests. cap() works on receive-only channels directly, so we
// can just return it; the helper exists to make intent explicit.
func asChan(t *testing.T, ch <-chan Event) <-chan Event {
	t.Helper()
	return ch
}

func TestBus_DroppedIncrementsWhenSubscriberFull(t *testing.T) {
	b := NewBus()
	// Buffer of 1: first publish fits, the next two get dropped (we never drain).
	_ = b.Subscribe(1)

	b.Publish(Event{Seq: 1})
	b.Publish(Event{Seq: 2})
	b.Publish(Event{Seq: 3})

	if got := b.Dropped(); got != 2 {
		t.Fatalf("Dropped = %d, want 2", got)
	}
}

func TestBus_DroppedIsPerSubscriber(t *testing.T) {
	b := NewBus()
	slow := b.Subscribe(1) // never drained beyond first
	fast := b.Subscribe(8) // drained immediately

	// Drain fast in the background; slow stays blocked at cap 1.
	done := make(chan struct{})
	var fastSeen int
	go func() {
		defer close(done)
		for range fast {
			fastSeen++
		}
	}()

	for i := 0; i < 5; i++ {
		b.Publish(Event{Seq: int64(i)})
	}

	// slow accepts 1 event, drops 4; fast accepts all 5.
	if got := b.Dropped(); got != 4 {
		t.Fatalf("Dropped = %d, want 4 (one per dropped publish on slow subscriber)", got)
	}

	b.Shutdown()
	<-done
	if fastSeen != 5 {
		t.Fatalf("fast subscriber saw %d events, want 5", fastSeen)
	}
	// Drain slow so we don't leak.
	for range slow {
	}
}

func TestBus_ShutdownClosesAllSubscribers(t *testing.T) {
	b := NewBus()
	a := b.Subscribe(4)
	c := b.Subscribe(4)

	b.Publish(Event{Seq: 1})
	b.Shutdown()

	// After Shutdown the channels are closed; ranging drains and exits.
	gotA, gotC := 0, 0
	for range a {
		gotA++
	}
	for range c {
		gotC++
	}
	if gotA != 1 || gotC != 1 {
		t.Fatalf("after Shutdown gotA=%d gotC=%d, want both 1", gotA, gotC)
	}
}

func TestBus_ConcurrentPublishAndSubscribe(t *testing.T) {
	b := NewBus()

	const (
		publishers           = 8
		eventsPerPublisher   = 200
		subscribers          = 4
		subscriberBufferSize = publishers * eventsPerPublisher // never drop
	)

	var received [subscribers]int64
	var drainers sync.WaitGroup
	chans := make([]<-chan Event, subscribers)
	for i := 0; i < subscribers; i++ {
		chans[i] = b.Subscribe(subscriberBufferSize)
	}
	for i := 0; i < subscribers; i++ {
		i := i
		drainers.Add(1)
		go func() {
			defer drainers.Done()
			for range chans[i] {
				atomic.AddInt64(&received[i], 1)
			}
		}()
	}

	var pubs sync.WaitGroup
	for p := 0; p < publishers; p++ {
		p := p
		pubs.Add(1)
		go func() {
			defer pubs.Done()
			for e := 0; e < eventsPerPublisher; e++ {
				b.Publish(Event{SessionID: "concurrent", Seq: int64(p*eventsPerPublisher + e)})
			}
		}()
	}
	pubs.Wait()

	b.Shutdown()
	drainers.Wait()

	want := int64(publishers * eventsPerPublisher)
	for i, got := range received {
		if got != want {
			t.Fatalf("subscriber %d received %d events, want %d", i, got, want)
		}
	}
	if got := b.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d, want 0 with adequate buffer", got)
	}
}

// TestBus_ConcurrentPublishAndSubscribeRegister exercises the RWMutex by
// registering new subscribers while publishers fire concurrently. We don't
// assert exact counts (late subscribers will miss earlier events); we only
// require that nothing races or deadlocks and that all goroutines exit.
func TestBus_ConcurrentPublishAndSubscribeRegister(t *testing.T) {
	b := NewBus()

	var pubs sync.WaitGroup
	stop := make(chan struct{})
	for p := 0; p < 4; p++ {
		pubs.Add(1)
		go func() {
			defer pubs.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(Event{Kind: GoalReceived})
				}
			}
		}()
	}

	// Race subscribers in while publishing.
	var subs sync.WaitGroup
	var drainers sync.WaitGroup
	chans := make([]<-chan Event, 0, 16)
	var chansMu sync.Mutex
	for s := 0; s < 16; s++ {
		subs.Add(1)
		go func() {
			defer subs.Done()
			ch := b.Subscribe(64)
			chansMu.Lock()
			chans = append(chans, ch)
			chansMu.Unlock()
			drainers.Add(1)
			go func() {
				defer drainers.Done()
				for range ch {
				}
			}()
		}()
	}
	subs.Wait()

	// Let publishers churn a touch.
	time.Sleep(20 * time.Millisecond)
	close(stop)
	pubs.Wait()

	b.Shutdown()
	drainers.Wait()
}

func TestBus_DroppedIsAtomicUnderLoad(t *testing.T) {
	b := NewBus()
	// Tiny buffer + no drainer guarantees drops once the buffer fills.
	_ = b.Subscribe(1)

	const publishers = 8
	const each = 500
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				b.Publish(Event{Seq: int64(i)})
			}
		}()
	}
	wg.Wait()

	total := uint64(publishers * each)
	// Exactly one slot ever fills before draining stops; the rest are drops.
	if got := b.Dropped(); got != total-1 {
		t.Fatalf("Dropped = %d, want %d", got, total-1)
	}
}
