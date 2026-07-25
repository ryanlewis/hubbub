package outbox

import (
	"log/slog"
	"sync"
)

// Outcome is a terminal per-channel result delivered to a waiting request
// handler (if it's still inside its response window) or settled in the
// delivery log by the worker otherwise.
type Outcome struct {
	Channel string
	Status  string // "ok" | "failed: <reason>" | "dropped: expired" | "dropped: evicted"
	OK      bool
}

type waiterKey struct{ reqID, channel string }

// Registry connects workers to the request handlers waiting out their
// response window. Resolve reports whether a waiter consumed the outcome —
// if not, the worker owns writing the terminal delivery-log line.
type Registry struct {
	mu      sync.Mutex
	waiters map[waiterKey]chan Outcome
}

func NewRegistry() *Registry {
	return &Registry{waiters: make(map[waiterKey]chan Outcome)}
}

// Add registers one aggregated outcome channel for reqID across channels.
// Must be called before Enqueue so a fast delivery can't miss its waiter.
func (r *Registry) Add(reqID string, channels []string) <-chan Outcome {
	c := make(chan Outcome, len(channels))
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range channels {
		k := waiterKey{reqID, ch}
		if _, taken := r.waiters[k]; taken {
			// Request ids are 128-bit, so this is unreachable short of a
			// crypto/rand failure — but overwriting would hand one request's
			// delivery outcome to another's handler. Keep the first waiter:
			// the loser reports "queued" at window close, which is safe.
			slog.Error("outbox waiter key already registered; not overwriting",
				"requestId", reqID, "channel", ch)
			continue
		}
		r.waiters[k] = c
	}
	return c
}

// Resolve hands a terminal outcome to the waiter if one is still registered.
// Returns true when a waiter consumed it (the handler will report/log it).
func (r *Registry) Resolve(reqID, channel string, o Outcome) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := waiterKey{reqID, channel}
	c, ok := r.waiters[k]
	if !ok {
		return false
	}
	delete(r.waiters, k)
	c <- o // buffered to len(channels); never blocks
	return true
}

// Cancel deregisters any remaining waiters for reqID at window close.
// Outcomes sent before Cancel are still buffered in the channel; the
// handler drains them non-blockingly after cancelling.
func (r *Registry) Cancel(reqID string, channels []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range channels {
		delete(r.waiters, waiterKey{reqID, ch})
	}
}
