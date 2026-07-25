// Package outbox is the delivery engine: every accepted notification is
// enqueued into a maildir-style spool and delivered by one serial worker
// goroutine per channel instance. The HTTP handler just waits on outcomes
// for the response window. A channel outage means late delivery, not loss.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ryanlewis/hubbub/internal/adapter"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/metrics"
	"github.com/ryanlewis/hubbub/internal/notify"
)

// stopGrace bounds how long SetChannels waits for a retired worker to finish
// its current attempt before its spool is purged.
const stopGrace = 15 * time.Second

// reapInterval is how often the spools of configured-but-disabled channels are
// swept for expired items. Disabling stops the worker that would otherwise
// apply the TTL, so without the sweep a parked channel's backlog outlives its
// TTL on disk with no terminal log line and every 202 it holds stays unsettled.
const reapInterval = time.Minute

// ChannelRuntime is what the engine needs to run one channel instance —
// deliberately not the config package's type, to keep packages acyclic.
//
// Disabled channels are passed too, with a nil Adapter: the engine has to
// tell "paused" (keep the spool) from "removed" (settle it and clean up),
// and it can only do that if it hears about both.
type ChannelRuntime struct {
	ID      string
	Enabled bool
	Adapter adapter.Adapter
}

type Options struct {
	SpoolDir       string
	TTL            time.Duration
	CapPerChannel  int
	DrainPace      time.Duration
	AttemptTimeout time.Duration
}

type Engine struct {
	opts    Options
	reg     *Registry
	log     *dlog.Logger
	metrics *metrics.Metrics

	mu      sync.Mutex
	workers map[string]*worker
	wanted  map[string]ChannelRuntime // every configured channel, enabled or not
	baseCtx context.Context
}

func NewEngine(ctx context.Context, opts Options, reg *Registry, log *dlog.Logger, m *metrics.Metrics) *Engine {
	e := &Engine{
		opts:    opts,
		reg:     reg,
		log:     log,
		metrics: m,
		workers: make(map[string]*worker),
		wanted:  make(map[string]ChannelRuntime),
		baseCtx: ctx,
	}
	go e.reapLoop()
	return e
}

// safeSpoolName reports whether id can be used as a single spool directory
// component. Config validates this too, but the engine joins the id straight
// into a filesystem path and a bad one is destructive: `".."` points a
// channel's spool at the config directory, whose files the drain scan then
// reads as zero-valued, long-expired items and deletes.
func safeSpoolName(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// SetChannels reconciles running workers with the (re)loaded channel set:
// enabled channels get a worker, disabled ones are stopped but keep their
// spool (disabling is a pause, not a purge), and channels that have left the
// config entirely have their backlog settled and their spool removed.
//
// Returns an error naming the channels whose spool could not be initialised.
// They stay wanted — Enqueue retries the init rather than leaving the channel
// dead until channels.json next changes.
func (e *Engine) SetChannels(chs []ChannelRuntime) error {
	e.mu.Lock()

	want := make(map[string]ChannelRuntime, len(chs))
	for _, ch := range chs {
		want[ch.ID] = ch
	}
	e.wanted = want

	var retired []*worker
	for id, w := range e.workers {
		if ch, ok := want[id]; !ok || !ch.Enabled {
			w.stop()
			retired = append(retired, w)
			delete(e.workers, id)
			slog.Info("outbox worker stopped", "channel", id)
		}
	}

	var errs []error
	for id, ch := range want {
		if !ch.Enabled {
			continue
		}
		if w, ok := e.workers[id]; ok {
			w.setAdapter(ch.Adapter)
			continue
		}
		if _, err := e.startWorkerLocked(ch); err != nil {
			errs = append(errs, fmt.Errorf("channel %q: %w", id, err))
		}
	}
	e.mu.Unlock()

	// Outside the lock: retired workers may still be inside an attempt, and
	// their spool must not be purged from under them.
	for _, w := range retired {
		w.wait(stopGrace)
	}
	e.purgeOrphanSpools(want)

	return errors.Join(errs...)
}

// startWorkerLocked builds the spool and starts the delivery goroutine.
// Caller holds e.mu.
func (e *Engine) startWorkerLocked(ch ChannelRuntime) (*worker, error) {
	if !safeSpoolName(ch.ID) {
		err := fmt.Errorf("channel id %q is not a usable spool directory name", ch.ID)
		slog.Error("outbox refused channel; channel inactive", "channel", ch.ID, "err", err)
		return nil, err
	}
	sp, err := newSpool(filepath.Join(e.opts.SpoolDir, ch.ID))
	if err != nil {
		slog.Error("outbox spool init failed; channel inactive", "channel", ch.ID, "err", err)
		return nil, err
	}
	w := newWorker(e.baseCtx, ch, sp, e.opts, e.reg, e.log, e.metrics)
	e.workers[ch.ID] = w
	go w.run()
	slog.Info("outbox worker started", "channel", ch.ID)
	return w, nil
}

// purgeOrphanSpools settles and clears the spool of any channel that is no
// longer configured at all. Without this a renamed or deleted channel's
// backlog sits on disk forever: never delivered, never expired, never given
// the terminal log line that settles its 202.
func (e *Engine) purgeOrphanSpools(want map[string]ChannelRuntime) {
	entries, err := os.ReadDir(e.opts.SpoolDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("spool dir scan failed", "dir", e.opts.SpoolDir, "err", err)
		}
		return
	}
	for _, ent := range entries {
		id := ent.Name()
		if !ent.IsDir() {
			continue
		}
		if _, ok := want[id]; ok {
			continue
		}
		if !safeSpoolName(id) {
			slog.Warn("spool dir with an unusable channel name left alone", "dir", id)
			continue
		}
		e.purgeSpool(id)
	}
}

func (e *Engine) purgeSpool(id string) {
	dir := filepath.Join(e.opts.SpoolDir, id)
	sp := &spool{dir: dir}
	names, err := sp.list()
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("orphan spool scan failed", "channel", id, "err", err)
		}
		return
	}
	const status = "dropped: channel removed"
	var purged int
	for _, name := range names {
		reqID, callerID := requestIDFromName(name), ""
		if it, err := sp.load(name); err == nil {
			reqID, callerID = it.Notification.RequestID, it.Notification.CallerID
		}
		// claim, not remove: a retired worker that outran its stop grace, or a
		// concurrent purge, may own this item's terminal line already.
		claimed, err := sp.claim(name)
		if err != nil {
			slog.Error("orphan spool item removal failed", "channel", id, "file", name, "err", err)
			continue
		}
		if !claimed {
			continue
		}
		purged++
		e.settle(id, reqID, callerID, status, "removed")
	}
	if purged > 0 {
		slog.Info("orphan spool purged", "channel", id, "messages", purged)
	}
	// Only succeeds once empty, which is what we want: leftovers keep the dir.
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		slog.Warn("orphan spool dir not removed", "channel", id, "err", err)
	}
}

// settle hands a terminal outcome to a waiting handler, or writes the delivery
// log line that settles the 202 when nobody is left waiting.
func (e *Engine) settle(id, reqID, callerID, status, metricOutcome string) {
	e.metrics.Delivery(id, metricOutcome)
	if e.reg.Resolve(reqID, id, Outcome{Channel: id, Status: status}) {
		return
	}
	e.log.Append(dlog.Record{
		Kind:      "terminal",
		RequestID: reqID,
		CallerID:  callerID,
		Channel:   id,
		Outcome:   status,
	})
}

// reapLoop applies the flat TTL to channels that are configured but disabled.
// Disabling is a pause, not a purge — the spool is kept deliberately — but it
// also stops the only goroutine that would ever expire that spool's contents.
func (e *Engine) reapLoop() {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-e.baseCtx.Done():
			return
		case <-t.C:
			e.reapDisabled()
		}
	}
}

func (e *Engine) reapDisabled() {
	e.mu.Lock()
	var ids []string
	for id, ch := range e.wanted {
		if !ch.Enabled && safeSpoolName(id) {
			ids = append(ids, id)
		}
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.expireSpool(id)
	}
}

// expireSpool settles every item in a stopped channel's spool that has outlived
// the TTL. It claims each file before settling, so re-enabling the channel
// mid-sweep can't produce two terminal lines for one message.
func (e *Engine) expireSpool(id string) {
	sp := &spool{dir: filepath.Join(e.opts.SpoolDir, id)}
	names, err := sp.list()
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("disabled spool scan failed", "channel", id, "err", err)
		}
		return
	}
	var expired int
	for _, name := range names {
		it, err := sp.load(name)
		if err != nil || time.Since(it.EnqueuedAt) <= e.opts.TTL {
			continue
		}
		if claimed, err := sp.claim(name); err != nil || !claimed {
			continue
		}
		expired++
		e.settle(id, it.Notification.RequestID, it.Notification.CallerID, "dropped: expired", "expired")
	}
	if expired > 0 {
		slog.Info("disabled channel backlog expired", "channel", id, "messages", expired)
	}
}

var ErrNoWorker = errors.New("channel has no running worker")

// Enqueue spools a notification for one channel and wakes its worker.
// Callers must register a waiter first (Registry.Add) if they intend to
// wait out the response window.
func (e *Engine) Enqueue(n notify.Notification, channel string, test bool) error {
	e.mu.Lock()
	w, ok := e.workers[channel]
	if !ok {
		// The channel is configured and enabled but has no worker, which means
		// its spool init failed earlier (disk full, wrong ownership after a
		// deploy). Retry now — otherwise the channel stays dead until
		// channels.json next changes, and every notification for it is lost
		// rather than queued.
		if ch, wanted := e.wanted[channel]; wanted && ch.Enabled {
			var err error
			if w, err = e.startWorkerLocked(ch); err != nil {
				e.mu.Unlock()
				return fmt.Errorf("%w (spool unavailable: %v)", ErrNoWorker, err)
			}
			ok = true
		}
	}
	e.mu.Unlock()
	if !ok {
		return ErrNoWorker
	}
	if err := w.enqueue(n, test); err != nil {
		return err
	}
	// SetChannels purges a removed channel's spool outside e.mu, so a purge
	// that scanned the directory before this write landed would leave the item
	// with no worker to deliver it and no terminal line to settle it. Re-check
	// and clean up here; purgeSpool claims each file, so racing the purge
	// itself can't double-settle.
	e.mu.Lock()
	_, stillWanted := e.wanted[channel]
	e.mu.Unlock()
	if !stillWanted {
		e.purgeSpool(channel)
	}
	return nil
}

// Shutdown stops every worker and waits for in-flight attempts to settle.
// A delivery that lands in the shutdown window has already unlinked its spool
// file by the time it writes its terminal line — so without this wait the
// process exits (or the delivery log closes) first and that outcome is lost
// from the only place a 202's promise gets settled, with nothing left on disk
// to re-settle at next boot.
func (e *Engine) Shutdown(timeout time.Duration) {
	e.mu.Lock()
	ws := make([]*worker, 0, len(e.workers))
	for id, w := range e.workers {
		w.stop()
		ws = append(ws, w)
		delete(e.workers, id)
	}
	e.mu.Unlock()

	deadline := time.Now().Add(timeout)
	for _, w := range ws {
		w.wait(max(time.Until(deadline), 0))
	}
}
