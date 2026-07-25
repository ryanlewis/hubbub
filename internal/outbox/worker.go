package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ryanlewis/hubbub/internal/adapter"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/metrics"
	"github.com/ryanlewis/hubbub/internal/notify"
)

// rateLimitFloor is the minimum wait after an upstream 429, whatever
// Retry-After asked for.
const rateLimitFloor = 30 * time.Second

// worker is the single serial delivery goroutine for one channel instance.
// Serial by design: exec scripts never race themselves, per-channel ordering
// is free, and cooldown/pacing state lives in exactly one goroutine.
type worker struct {
	channelID string
	adapter   atomic.Pointer[adapterHolder] // swapped on config reload
	sp        *spool
	opts      Options
	reg       *Registry
	log       *dlog.Logger
	metrics   *metrics.Metrics

	wake chan struct{}
	// ctx/cancel are built by newWorker, before run's goroutine exists, so
	// stop() can never observe a half-constructed worker.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed when run returns

	mu       sync.Mutex           // guards enqueue/eviction against the in-flight item
	inFlight string               // spool file currently being delivered ("" when idle)
	stuck    map[string]struct{}  // delivered but un-removable: never re-send
	hold     map[string]time.Time // authoritative not-before, in case the rewrite didn't land
}

type adapterHolder struct{ a adapter.Adapter }

func newWorker(parent context.Context, ch ChannelRuntime, sp *spool, opts Options, reg *Registry, log *dlog.Logger, m *metrics.Metrics) *worker {
	ctx, cancel := context.WithCancel(parent)
	w := &worker{
		channelID: ch.ID,
		sp:        sp,
		opts:      opts,
		reg:       reg,
		log:       log,
		metrics:   m,
		wake:      make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		stuck:     make(map[string]struct{}),
		hold:      make(map[string]time.Time),
	}
	w.adapter.Store(&adapterHolder{a: ch.Adapter})
	return w
}

func (w *worker) setAdapter(a adapter.Adapter) {
	w.adapter.Store(&adapterHolder{a: a})
}

// stop signals the worker to exit. It does not block; callers that need the
// goroutine actually gone (before purging its spool, say) then wait().
func (w *worker) stop() { w.cancel() }

// wait blocks until run has returned, or the timeout elapses.
func (w *worker) wait(timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-w.done:
	case <-t.C:
		slog.Warn("outbox worker did not stop in time", "channel", w.channelID)
	}
}

func (w *worker) kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// enqueue spools an item, evicting the oldest normal message when full —
// fresh news beats stale news, and nothing drops without a log line.
func (w *worker) enqueue(n notify.Notification, test bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if cnt, err := w.sp.count(); err == nil && cnt >= w.opts.CapPerChannel {
		// Never evict the item the worker is mid-Send on: its delivery is
		// already under way, and settling it as "dropped" would report a
		// failure for a message the upstream is about to accept. Nor a stuck
		// one: that was delivered and settled "ok" already, and evicting it now
		// would append a second, contradictory terminal line under the same
		// request id — and count the notification twice in the metrics.
		skip := make(map[string]struct{}, len(w.stuck)+1)
		for name := range w.stuck {
			skip[name] = struct{}{}
		}
		if w.inFlight != "" {
			skip[w.inFlight] = struct{}{}
		}
		victim, err := w.sp.oldestEvictable(skip)
		switch {
		case err != nil:
			slog.Error("spool eviction scan failed", "channel", w.channelID, "err", err)
		case victim != "":
			w.finish(victim, requestIDFromName(victim), "",
				Outcome{Channel: w.channelID, Status: "dropped: evicted"}, "evicted")
		default:
			// Nothing evictable — everything on disk is in flight or already
			// delivered. Admit anyway and sit over cap until they clear.
		}
	}

	it := &Item{
		Notification: n,
		Channel:      w.channelID,
		EnqueuedAt:   time.Now().UTC(),
		NotBefore:    time.Now().UTC(),
		Test:         test,
	}
	if err := w.sp.put(it); err != nil {
		return err
	}
	w.kick()
	return nil
}

func (w *worker) run() {
	defer close(w.done)
	defer w.cancel()

	for {
		name, it, wait := w.next()
		if it == nil {
			select {
			case <-w.ctx.Done():
				return
			case <-w.wake:
			case <-time.After(wait):
			}
			continue
		}
		w.process(w.ctx, name, it)
		if w.ctx.Err() != nil {
			return
		}
		// Paced drain: if a backlog remains, trickle rather than storm.
		if cnt, err := w.sp.count(); err == nil && cnt > 0 {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(w.opts.DrainPace):
			}
		}
	}
}

// next picks the first eligible item in queue order and claims it as
// in-flight, or returns how long to sleep until one becomes eligible. Holds
// w.mu for the scan so a concurrent enqueue can't evict what it just claimed.
func (w *worker) next() (string, *Item, time.Duration) {
	const idleWait = time.Second
	w.mu.Lock()
	defer w.mu.Unlock()

	names, err := w.sp.list()
	if err != nil {
		slog.Error("spool list failed", "channel", w.channelID, "err", err)
		return "", nil, idleWait
	}
	w.pruneHoldLocked(names)
	wait := idleWait
	now := time.Now()
	for _, name := range names {
		// Already delivered, but the file wouldn't delete. Retry the removal;
		// never re-send.
		if _, done := w.stuck[name]; done {
			if err := w.sp.remove(name); err == nil {
				delete(w.stuck, name)
			}
			continue
		}
		it, err := w.sp.load(name)
		if err != nil {
			if errors.Is(err, errCorrupt) {
				slog.Error("spool item corrupt; dropping", "channel", w.channelID, "file", name, "err", err)
				w.finish(name, requestIDFromName(name), "",
					Outcome{Channel: w.channelID, Status: "dropped: corrupt"}, "corrupt")
				continue
			}
			// Transient read failure — fd exhaustion, EIO, permissions changed
			// under the process. Leave the message spooled; deleting here
			// would wipe an entire backlog in one scan.
			slog.Error("spool item unreadable; leaving queued", "channel", w.channelID, "file", name, "err", err)
			continue
		}
		// w.hold wins where it's later: if the rewrite that persisted a backoff
		// failed, the file on disk still carries a not-before in the past.
		notBefore := it.NotBefore
		if h, held := w.hold[name]; held && h.After(notBefore) {
			notBefore = h
		}
		if notBefore.After(now) {
			if d := time.Until(notBefore); d < wait {
				wait = d
			}
			continue
		}
		w.inFlight = name
		return name, it, 0
	}
	return "", nil, wait
}

// pruneHoldLocked drops held not-befores for items that have left the spool,
// so the map can't outgrow the queue. Caller holds w.mu.
func (w *worker) pruneHoldLocked(names []string) {
	if len(w.hold) == 0 {
		return
	}
	live := make(map[string]struct{}, len(names))
	for _, n := range names {
		live[n] = struct{}{}
	}
	for n := range w.hold {
		if _, ok := live[n]; !ok {
			delete(w.hold, n)
		}
	}
}

func (w *worker) process(ctx context.Context, name string, it *Item) {
	defer func() {
		w.mu.Lock()
		w.inFlight = ""
		w.mu.Unlock()
	}()

	// Flat TTL for every message, test sends included: a per-class exemption
	// would make test items immortal, since eviction also passes over them
	// while real traffic is queued.
	if time.Since(it.EnqueuedAt) > w.opts.TTL {
		w.finishItem(name, it, Outcome{Channel: w.channelID, Status: "dropped: expired"}, "expired")
		return
	}

	attemptCtx, cancel := context.WithTimeout(ctx, w.opts.AttemptTimeout)
	err := w.adapter.Load().a.Send(attemptCtx, it.Notification)
	cancel()

	if err == nil {
		o := Outcome{Channel: w.channelID, Status: "ok", OK: true}
		if !w.finishItem(name, it, o, "ok") {
			// Delivered, but the spool file survived. Remember it so the drain
			// loop doesn't re-send the same notification every DrainPace until
			// someone restarts the process.
			w.mu.Lock()
			w.stuck[name] = struct{}{}
			w.mu.Unlock()
		}
		return
	}

	var se *adapter.SendError
	if !errors.As(err, &se) {
		se = adapter.Retryable("%v", err)
	}
	// A cancelled attempt (shutdown / config reload) is not a verdict on the
	// message: leave it spooled untouched for the next worker.
	if ctx.Err() != nil {
		return
	}

	switch se.Kind {
	case adapter.KindPermanent:
		w.finishItem(name, it, Outcome{Channel: w.channelID, Status: "failed: " + se.Reason}, "failed")

	case adapter.KindRateLimited:
		// Terminal per attempt: never sleep a caller's window away. The
		// not-before honours Retry-After across requests, floored — a
		// "Retry-After: 0" or a date already past (a few seconds of clock skew
		// is enough) would otherwise re-fire at DrainPace against the server
		// that just throttled us.
		it.Attempts++
		floor := time.Now().Add(rateLimitFloor)
		it.NotBefore = se.NotBefore
		if it.NotBefore.Before(floor) {
			it.NotBefore = floor
		}
		w.reschedule(name, it)
		w.metrics.Delivery(w.channelID, "rate_limited")

	default: // retryable
		it.Attempts++
		it.NotBefore = time.Now().Add(backoff(it.Attempts))
		w.reschedule(name, it)
		w.metrics.Delivery(w.channelID, "retry")
	}
}

// reschedule caps an item's new not-before at its TTL deadline, records it in
// memory and persists it.
//
// The cap keeps the flat TTL enforceable. next() skips any item whose
// not-before is still in the future, before process() ever reaches the TTL
// check — so an unbounded Retry-After ("Retry-After: 86400" is a real
// free-tier answer) would park the message well past its own expiry, holding a
// spool slot and settling the caller's 202 hours after it was contractually
// dead. Capped, it wakes at the deadline and drops as expired.
//
// The in-memory copy is what next() honours: a rewrite that fails (spool full,
// or read-only after a deploy) would otherwise leave a past not-before on disk
// and the worker would re-fire every DrainPace at the server that just
// throttled it — the exact storm the floor exists to prevent.
func (w *worker) reschedule(name string, it *Item) {
	if deadline := it.EnqueuedAt.Add(w.opts.TTL); it.NotBefore.After(deadline) {
		it.NotBefore = deadline
	}
	w.mu.Lock()
	w.hold[name] = it.NotBefore
	w.mu.Unlock()
	if err := w.sp.rewrite(name, it); err != nil {
		slog.Error("spool rewrite failed; holding not-before in memory",
			"channel", w.channelID, "file", name, "notBefore", it.NotBefore, "err", err)
	}
}

func (w *worker) finishItem(name string, it *Item, o Outcome, metricOutcome string) bool {
	return w.finish(name, it.Notification.RequestID, it.Notification.CallerID, o, metricOutcome)
}

// finish unspools an item and settles its terminal outcome. Removal comes
// first, and its error is not ignored: reporting "dropped" for a message
// still on disk is a lie the next drain loop exposes by delivering it anyway.
// Reports whether the file is actually gone.
//
// Safe to call with or without w.mu held — it touches no worker state.
func (w *worker) finish(name, reqID, callerID string, o Outcome, metricOutcome string) bool {
	if err := w.sp.remove(name); err != nil {
		slog.Error("spool remove failed", "channel", w.channelID, "file", name, "outcome", o.Status, "err", err)
		if !o.OK {
			// Not delivered and not removed: it's still queued, so don't
			// settle a terminal outcome the caller would act on.
			return false
		}
		// Delivered: the send genuinely happened, so the caller is owed its
		// outcome even though cleanup failed.
		w.metrics.Delivery(w.channelID, metricOutcome)
		w.settle(reqID, callerID, o)
		return false
	}
	w.metrics.Delivery(w.channelID, metricOutcome)
	w.settle(reqID, callerID, o)
	return true
}

// settle hands a terminal outcome to a waiting handler; if none is waiting
// (window closed, or a restart orphaned the item), the worker owns the
// terminal delivery-log line — the log is where a 202's promise is settled.
func (w *worker) settle(reqID, callerID string, o Outcome) {
	if w.reg.Resolve(reqID, w.channelID, o) {
		return
	}
	w.log.Append(dlog.Record{
		Kind:      "terminal",
		RequestID: reqID,
		CallerID:  callerID,
		Channel:   w.channelID,
		Outcome:   o.Status,
	})
}

// backoff: 30s, 1m, 2m, then 5m capped. Tuning, not contract.
func backoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 30 * time.Second
	case attempts == 2:
		return time.Minute
	case attempts == 3:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}
