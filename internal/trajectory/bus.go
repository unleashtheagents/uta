package trajectory

import "sync"

// Bus is an in-process fan-out for trajectory events. Producers (the
// supervisor) call Publish; consumers register via Subscribe. Each subscriber
// gets its own buffered channel — a slow subscriber slows nobody else as long
// as its buffer isn't full; if it is, Publish drops that subscriber's copy
// and continues. The recorder is the canonical durable subscriber.
type Bus struct {
	mu   sync.RWMutex
	subs []chan Event
}

func NewBus() *Bus { return &Bus{} }

// Subscribe returns a channel that will receive every subsequent event.
// Callers must drain it; closing is the Bus's responsibility on Shutdown.
func (b *Bus) Subscribe(buffer int) <-chan Event {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch
}

// Publish fans the event out to every subscriber. Non-blocking per subscriber:
// if a subscriber's buffer is full, that one copy is dropped (a future
// "slowness watchdog" can detect chronic drops).
func (b *Bus) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Shutdown closes every subscriber channel. Bus must not be used after.
func (b *Bus) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = nil
}
