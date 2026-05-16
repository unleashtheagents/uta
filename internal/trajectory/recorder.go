package trajectory

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/unleashtheagents/uta/internal/store"
)

// Recorder is the canonical durable subscriber on the Bus. It owns one
// monotonic `seq` counter per session and writes every event to SQLite.
//
// Layering note: Recorder lives in the trajectory package and imports store
// because that direction is allowed (trajectory is application-level wiring).
// Producers (the supervisor) only import the bus/event types.
type Recorder struct {
	store *store.Store

	mu      sync.Mutex
	seqs    map[string]*int64 // session_id → next seq (atomic-incremented)
	dropped uint64            // count of events we failed to persist
}

func NewRecorder(s *store.Store) *Recorder {
	return &Recorder{store: s, seqs: map[string]*int64{}}
}

// Run subscribes to bus and drains events into SQLite until ctx is done or
// the bus closes its channel. Designed to be called as `go recorder.Run(...)`.
func (r *Recorder) Run(ctx context.Context, bus *Bus) {
	ch := bus.Subscribe(256)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			r.persist(ev)
		}
	}
}

// PersistSync inserts one event immediately (bypassing the bus). Used by the
// supervisor for ordering-critical events like run_completed where we want
// the row visible before the call returns.
func (r *Recorder) PersistSync(ev Event) { r.persist(ev) }

// AllocSeq returns the next monotonic seq for a session, allocating the
// counter on first use.
func (r *Recorder) AllocSeq(sessionID string) int64 {
	r.mu.Lock()
	ctr, ok := r.seqs[sessionID]
	if !ok {
		var zero int64
		ctr = &zero
		r.seqs[sessionID] = ctr
	}
	r.mu.Unlock()
	return atomic.AddInt64(ctr, 1)
}

// Dropped reports the number of events whose insert failed (DB errors). Used
// by `uta doctor` and tests; never resets.
func (r *Recorder) Dropped() uint64 { return atomic.LoadUint64(&r.dropped) }

func (r *Recorder) persist(ev Event) {
	if ev.SessionID == "" {
		atomic.AddUint64(&r.dropped, 1)
		return
	}
	if ev.Seq == 0 {
		ev.Seq = r.AllocSeq(ev.SessionID)
	}
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	if err := r.store.InsertEvent(ev.SessionID, ev.SubtaskID, ev.Seq, ev.Ts, string(ev.Kind), payload); err != nil {
		atomic.AddUint64(&r.dropped, 1)
	}
}
