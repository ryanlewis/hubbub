package httpapi

import (
	"sync"
	"time"
)

// RateLimiter is the v1 global cap: one in-memory rolling window across all
// keys — a blast-radius guard against a runaway caller, not accounting.
// Resets on restart by design. Per-key caps land the day key #2 is issued.
type RateLimiter struct {
	mu     sync.Mutex
	cap    int
	window time.Duration
	times  []time.Time
}

func NewRateLimiter(capPerWindow int, window time.Duration) *RateLimiter {
	return &RateLimiter{cap: capPerWindow, window: window}
}

// Allow records one request if under the cap; otherwise it reports how long
// until the oldest recorded request falls out of the window (Retry-After).
func (r *RateLimiter) Allow(now time.Time) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A non-positive cap is rejected by config.LoadConfig; guard anyway so a
	// programmatic zero degrades to "uncapped" rather than indexing an empty
	// slice on the first request and panicking every notify from then on.
	if r.cap <= 0 {
		return true, 0
	}

	cutoff := now.Add(-r.window)
	keep := r.times[:0]
	for _, t := range r.times {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	r.times = keep

	if len(r.times) >= r.cap {
		retry := r.times[0].Sub(cutoff)
		if retry < time.Second {
			retry = time.Second
		}
		return false, retry
	}
	r.times = append(r.times, now)
	return true, 0
}
