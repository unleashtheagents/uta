package builtin

import (
	"io"
	"sync"

	"github.com/unleashtheagents/uta/internal/provider"
)

// safeEmit sends an event to events if the channel is non-nil; if the
// receiver isn't draining, we still proceed (events is best-effort here, the
// recorder is the durable subscriber via the bus).
func safeEmit(events chan<- provider.Event, ev provider.Event) {
	if events == nil {
		return
	}
	select {
	case events <- ev:
	default:
		// drop if subscriber slow; we never block the parser.
	}
}

// lockedWriter serializes writes to an io.Writer (single-threaded today,
// but cheap insurance against any future concurrent tee'ing).
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
