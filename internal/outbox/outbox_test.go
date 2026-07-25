package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanlewis/hubbub/internal/adapter"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/metrics"
	"github.com/ryanlewis/hubbub/internal/notify"
)

// fakeAdapter records calls and returns scripted errors.
type fakeAdapter struct {
	mu    sync.Mutex
	calls []string // request ids in delivery order
	fn    func(n notify.Notification) error
}

func (f *fakeAdapter) Send(_ context.Context, n notify.Notification) error {
	f.mu.Lock()
	f.calls = append(f.calls, n.RequestID)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(n)
	}
	return nil
}

func testOpts(dir string) Options {
	return Options{
		SpoolDir:       dir,
		TTL:            time.Hour,
		CapPerChannel:  100,
		DrainPace:      time.Millisecond,
		AttemptTimeout: time.Second,
	}
}

func testLogger(t *testing.T, dir string) *dlog.Logger {
	t.Helper()
	l, err := dlog.Open(filepath.Join(dir, "delivery.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func readLog(t *testing.T, dir string) []dlog.Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "delivery.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var recs []dlog.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r dlog.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

func newTestWorker(t *testing.T, dir string, fa adapter.Adapter, opts Options) (*worker, *Registry) {
	t.Helper()
	sp, err := newSpool(filepath.Join(dir, "ch"))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := newWorker(ctx, ChannelRuntime{ID: "ch", Enabled: true, Adapter: fa}, sp, opts, reg, testLogger(t, dir), metrics.New())
	return w, reg
}

func note(id string) notify.Notification {
	return notify.Notification{Title: "t", Message: "m", Priority: notify.PriorityDefault, RequestID: id, CallerID: "test"}
}

func TestDeliverResolvesWaiter(t *testing.T) {
	dir := t.TempDir()
	w, reg := newTestWorker(t, dir, &fakeAdapter{}, testOpts(dir))
	go w.run()

	outcomes := reg.Add("r_1", []string{"ch"})
	if err := w.enqueue(note("r_1"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-outcomes:
		if !o.OK || o.Status != "ok" {
			t.Errorf("outcome = %+v", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome within window")
	}
	if cnt, _ := w.sp.count(); cnt != 0 {
		t.Errorf("spool should be empty, has %d", cnt)
	}
}

func TestPermanentFailureResolvesFailed(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.Permanent("bad topic") }}
	w, reg := newTestWorker(t, dir, fa, testOpts(dir))
	go w.run()

	outcomes := reg.Add("r_2", []string{"ch"})
	_ = w.enqueue(note("r_2"), false)
	o := <-outcomes
	if o.OK || o.Status != "failed: bad topic" {
		t.Errorf("outcome = %+v", o)
	}
	if cnt, _ := w.sp.count(); cnt != 0 {
		t.Error("permanent failure must not stay spooled")
	}
}

func TestRetryableStaysSpooledWithBackoff(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.Retryable("gateway 503") }}
	w, _ := newTestWorker(t, dir, fa, testOpts(dir))
	go w.run()

	_ = w.enqueue(note("r_3"), false)
	time.Sleep(300 * time.Millisecond)

	names, _ := w.sp.list()
	if len(names) != 1 {
		t.Fatalf("spool = %v, want 1 item", names)
	}
	it, err := w.sp.load(names[0])
	if err != nil {
		t.Fatal(err)
	}
	if it.Attempts < 1 {
		t.Errorf("attempts = %d, want >=1", it.Attempts)
	}
	if !it.NotBefore.After(time.Now().Add(10 * time.Second)) {
		t.Errorf("notBefore = %v, want ~30s out", it.NotBefore)
	}
}

func TestRateLimitedSetsNotBefore(t *testing.T) {
	dir := t.TempDir()
	nb := time.Now().Add(time.Hour).UTC()
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.RateLimited(nb, "429") }}
	w, _ := newTestWorker(t, dir, fa, testOpts(dir))
	go w.run()

	_ = w.enqueue(note("r_4"), false)
	time.Sleep(300 * time.Millisecond)

	names, _ := w.sp.list()
	if len(names) != 1 {
		t.Fatalf("spool = %v, want 1 item", names)
	}
	it, _ := w.sp.load(names[0])
	if !it.NotBefore.Equal(nb) {
		t.Errorf("notBefore = %v, want %v (Retry-After honoured as not-before)", it.NotBefore, nb)
	}
	fa.mu.Lock()
	calls := len(fa.calls)
	fa.mu.Unlock()
	if calls != 1 {
		t.Errorf("adapter called %d times; 429 is terminal per attempt", calls)
	}
}

func TestTTLExpiryDropsWithLogLine(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.TTL = 50 * time.Millisecond
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.Retryable("still down") }}
	w, _ := newTestWorker(t, dir, fa, opts)

	// Pre-spool an item already past its TTL.
	it := &Item{Notification: note("r_5"), Channel: "ch", EnqueuedAt: time.Now().Add(-time.Minute), NotBefore: time.Now().Add(-time.Minute)}
	if err := w.sp.put(it); err != nil {
		t.Fatal(err)
	}
	go w.run()
	time.Sleep(300 * time.Millisecond)

	if cnt, _ := w.sp.count(); cnt != 0 {
		t.Error("expired item must be dropped")
	}
	var found bool
	for _, r := range readLog(t, dir) {
		if r.Kind == "terminal" && r.RequestID == "r_5" && r.Outcome == "dropped: expired" {
			found = true
		}
	}
	if !found {
		t.Error("expiry must settle in the delivery log (nothing dropped without a log line)")
	}
}

func TestEvictOldestWhenFull(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.CapPerChannel = 2
	w, _ := newTestWorker(t, dir, &fakeAdapter{}, opts) // worker NOT running

	_ = w.enqueue(note("r_a"), false)
	time.Sleep(2 * time.Millisecond) // distinct enqueue nanos
	_ = w.enqueue(note("r_b"), false)
	time.Sleep(2 * time.Millisecond)
	_ = w.enqueue(note("r_c"), false)

	names, _ := w.sp.list()
	if len(names) != 2 {
		t.Fatalf("spool = %d items, want 2 (evict-oldest)", len(names))
	}
	for _, n := range names {
		it, _ := w.sp.load(n)
		if it.Notification.RequestID == "r_a" {
			t.Error("oldest (r_a) should have been evicted")
		}
	}
	var found bool
	for _, r := range readLog(t, dir) {
		if r.Kind == "terminal" && r.RequestID == "r_a" && r.Outcome == "dropped: evicted" {
			found = true
		}
	}
	if !found {
		t.Error("eviction must settle in the delivery log")
	}
}

func TestTestSendsJumpTheQueue(t *testing.T) {
	dir := t.TempDir()
	w, _ := newTestWorker(t, dir, &fakeAdapter{}, testOpts(dir)) // worker NOT running yet

	_ = w.enqueue(note("r_normal"), false)
	time.Sleep(2 * time.Millisecond)
	_ = w.enqueue(note("r_test"), true)

	names, _ := w.sp.list()
	if len(names) != 2 || !strings.HasPrefix(names[0], "0-") {
		t.Fatalf("test send must sort first: %v", names)
	}
	it, _ := w.sp.load(names[0])
	if it.Notification.RequestID != "r_test" {
		t.Errorf("head of queue = %s, want r_test", it.Notification.RequestID)
	}
}

// Eviction is plain oldest-first across both classes. Exempting test sends
// gave the ops port — unauthenticated, and deliberately uncapped — a way to
// evict an entire real backlog one test fire at a time.
func TestEvictionDoesNotSpareTestSends(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.CapPerChannel = 2
	w, _ := newTestWorker(t, dir, &fakeAdapter{}, opts) // worker NOT running

	_ = w.enqueue(note("r_test"), true) // oldest
	time.Sleep(2 * time.Millisecond)
	_ = w.enqueue(note("r_real_a"), false)
	time.Sleep(2 * time.Millisecond)
	_ = w.enqueue(note("r_real_b"), false) // over cap

	names, _ := w.sp.list()
	if len(names) != 2 {
		t.Fatalf("spool = %d items, want 2", len(names))
	}
	for _, n := range names {
		it, _ := w.sp.load(n)
		if it.Test {
			t.Error("the oldest item was a test send and should have been the victim")
		}
	}
	var evicted string
	for _, r := range readLog(t, dir) {
		if r.Outcome == "dropped: evicted" {
			evicted = r.RequestID
		}
	}
	if evicted != "r_test" {
		t.Errorf("evicted %q, want r_test (oldest); a real alert was destroyed by diagnostic traffic", evicted)
	}
}

// A delivered-but-un-removable item is already settled "ok". Evicting it later
// unlinks the file successfully and appends a second, contradictory terminal
// line under the same request id — and counts the notification twice.
func TestEvictionSkipsAnAlreadyDeliveredItem(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.CapPerChannel = 2
	w, _ := newTestWorker(t, dir, &fakeAdapter{}, opts) // worker NOT running

	_ = w.enqueue(note("r_stuck"), false) // oldest
	time.Sleep(2 * time.Millisecond)
	_ = w.enqueue(note("r_live"), false)

	// r_stuck was delivered and settled, but its file wouldn't unlink.
	names, _ := w.sp.list()
	w.mu.Lock()
	w.stuck[names[0]] = struct{}{}
	w.mu.Unlock()

	_ = w.enqueue(note("r_new"), false) // over cap

	for _, r := range readLog(t, dir) {
		if r.RequestID == "r_stuck" && r.Outcome == "dropped: evicted" {
			t.Error("an already-delivered message was evicted: it now reads as both ok and dropped in the log")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ch", names[0])); os.IsNotExist(err) {
		t.Error("the delivered item's file was unlinked by eviction rather than by its own retry")
	}
}

// An unbounded Retry-After ("Retry-After: 86400" is a real free-tier answer)
// parked a message past its own TTL: next() skips a future not-before before
// process() ever reaches the expiry check, so the caller's 202 settled hours
// after the message was contractually dead.
func TestRateLimitedNotBeforeIsCappedAtTheTTL(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.TTL = time.Minute
	fa := &fakeAdapter{fn: func(notify.Notification) error {
		return adapter.RateLimited(time.Now().Add(24*time.Hour), "429")
	}}
	w, _ := newTestWorker(t, dir, fa, opts)
	go w.run()

	_ = w.enqueue(note("r_rl"), false)
	time.Sleep(300 * time.Millisecond)

	names, _ := w.sp.list()
	if len(names) != 1 {
		t.Fatalf("spool = %v, want the message still queued", names)
	}
	it, _ := w.sp.load(names[0])
	deadline := it.EnqueuedAt.Add(opts.TTL)
	if it.NotBefore.After(deadline) {
		t.Errorf("notBefore = %v, past the TTL deadline %v: the message outlives its own expiry", it.NotBefore, deadline)
	}
}

// A rewrite that fails leaves the old, already-past not-before on disk. Unless
// the backoff is also held in memory the worker re-fires every DrainPace at
// the server that just throttled it.
func TestBackoffSurvivesAFailedRewrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.RateLimited(time.Now(), "429") }}
	w, _ := newTestWorker(t, dir, fa, testOpts(dir))
	spoolDir := filepath.Join(dir, "ch")

	_ = w.enqueue(note("r_rw"), false)
	// Read+execute only: the item can be read, but no rewrite can land.
	if err := os.Chmod(spoolDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(spoolDir, 0o700) })

	go w.run()
	time.Sleep(300 * time.Millisecond) // ~300 DrainPace ticks

	fa.mu.Lock()
	calls := len(fa.calls)
	fa.mu.Unlock()
	if calls != 1 {
		t.Errorf("adapter called %d times; a backoff whose rewrite failed must still be honoured", calls)
	}
}

func TestRegistryResolveAfterCancel(t *testing.T) {
	reg := NewRegistry()
	outcomes := reg.Add("r_x", []string{"a", "b"})
	if !reg.Resolve("r_x", "a", Outcome{Channel: "a", Status: "ok", OK: true}) {
		t.Error("resolve with live waiter must report consumed")
	}
	reg.Cancel("r_x", []string{"a", "b"})
	if reg.Resolve("r_x", "b", Outcome{Channel: "b", Status: "ok"}) {
		t.Error("resolve after cancel must report unconsumed (worker owns the log line)")
	}
	if o := <-outcomes; o.Channel != "a" {
		t.Errorf("buffered outcome = %+v", o)
	}
}

// Eviction must never pick the message the worker is mid-Send on: settling it
// as "dropped: evicted" reports a failure for a message the upstream is about
// to accept, so the caller retries and the phone buzzes twice.
func TestEvictionSkipsTheInFlightItem(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.CapPerChannel = 1

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	fa := &fakeAdapter{fn: func(notify.Notification) error {
		started <- struct{}{}
		<-release
		return nil
	}}
	w, reg := newTestWorker(t, dir, fa, opts)

	outA := reg.Add("r_a", []string{"ch"})
	if err := w.enqueue(note("r_a"), false); err != nil {
		t.Fatal(err)
	}
	go w.run()
	<-started // r_a is now inside Send, with its spool file still on disk

	// A second message arrives with the spool at cap.
	if err := w.enqueue(note("r_b"), false); err != nil {
		t.Fatal(err)
	}
	close(release)

	select {
	case o := <-outA:
		if o.Status != "ok" {
			t.Errorf("in-flight message settled as %q, want ok", o.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome for the in-flight message")
	}
	for _, r := range readLog(t, dir) {
		if r.RequestID == "r_a" && r.Outcome == "dropped: evicted" {
			t.Error("the in-flight message was evicted mid-delivery")
		}
	}
}

// "Retry-After: 0" and dates already in the past both parse to "now". Without
// a floor the worker re-fires at DrainPace against the server that just
// throttled it.
func TestRateLimitedFloorsAnImmediateRetryAfter(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAdapter{fn: func(notify.Notification) error {
		return adapter.RateLimited(time.Now(), "429")
	}}
	w, _ := newTestWorker(t, dir, fa, testOpts(dir))
	go w.run()

	_ = w.enqueue(note("r_rl"), false)
	time.Sleep(300 * time.Millisecond) // ~300 DrainPace ticks

	fa.mu.Lock()
	calls := len(fa.calls)
	fa.mu.Unlock()
	if calls != 1 {
		t.Errorf("adapter called %d times; a non-positive Retry-After must still back off", calls)
	}
	names, _ := w.sp.list()
	if len(names) != 1 {
		t.Fatalf("spool = %v, want the message still queued", names)
	}
	it, _ := w.sp.load(names[0])
	if !it.NotBefore.After(time.Now().Add(20 * time.Second)) {
		t.Errorf("notBefore = %v, want the 30s floor applied", it.NotBefore)
	}
}

// The TTL is flat: a test send that can't be delivered must age out like any
// other message rather than occupying a spool slot indefinitely.
func TestTestSendsExpireLikeAnythingElse(t *testing.T) {
	dir := t.TempDir()
	opts := testOpts(dir)
	opts.TTL = 50 * time.Millisecond
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.Retryable("still down") }}
	w, _ := newTestWorker(t, dir, fa, opts)

	it := &Item{
		Notification: note("r_test"), Channel: "ch", Test: true,
		EnqueuedAt: time.Now().Add(-time.Hour), NotBefore: time.Now().Add(-time.Hour),
	}
	if err := w.sp.put(it); err != nil {
		t.Fatal(err)
	}
	go w.run()
	time.Sleep(300 * time.Millisecond)

	if cnt, _ := w.sp.count(); cnt != 0 {
		t.Error("an expired test send must be dropped, not kept forever")
	}
}

// A corrupt file is deleted (with its log line, recovered from the filename);
// a file that merely won't read right now is left alone. Under fd exhaustion
// the old code deleted an entire backlog in one scan.
func TestUnreadableSpoolItemSurvivesButCorruptOneDrops(t *testing.T) {
	dir := t.TempDir()
	w, _ := newTestWorker(t, dir, &fakeAdapter{}, testOpts(dir))
	spoolDir := filepath.Join(dir, "ch")

	corrupt := "1-00000000000000000001-r_corrupt.json"
	if err := os.WriteFile(filepath.Join(spoolDir, corrupt), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := "1-00000000000000000002-r_locked.json"
	if err := os.WriteFile(filepath.Join(spoolDir, unreadable), []byte(`{"channel":"ch"}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(spoolDir, unreadable), 0o600) })

	go w.run()
	time.Sleep(200 * time.Millisecond)

	if _, err := os.Stat(filepath.Join(spoolDir, corrupt)); !os.IsNotExist(err) {
		t.Error("a corrupt item should be dropped")
	}
	var logged bool
	for _, r := range readLog(t, dir) {
		if r.RequestID == "r_corrupt" && r.Outcome == "dropped: corrupt" {
			logged = true
		}
	}
	if !logged {
		t.Error("a corrupt drop still needs its terminal log line (id recovered from the filename)")
	}
	if os.Geteuid() == 0 {
		return // root ignores file permissions
	}
	if _, err := os.Stat(filepath.Join(spoolDir, unreadable)); err != nil {
		t.Error("a transient read failure must leave the message queued, not delete it")
	}
}

// Delivered, but the spool file wouldn't unlink: the send must not repeat
// every DrainPace until someone restarts the process.
func TestDeliveredItemThatCannotBeRemovedIsNotResent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	w, reg := newTestWorker(t, dir, &fakeAdapter{}, testOpts(dir))
	spoolDir := filepath.Join(dir, "ch")

	outcomes := reg.Add("r_stuck", []string{"ch"})
	if err := w.enqueue(note("r_stuck"), false); err != nil {
		t.Fatal(err)
	}
	// Read+execute only: the file can be read and written, but not unlinked.
	if err := os.Chmod(spoolDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(spoolDir, 0o700) })

	go w.run()
	select {
	case o := <-outcomes:
		if !o.OK {
			t.Fatalf("outcome = %+v, want ok (the send really happened)", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome")
	}
	time.Sleep(300 * time.Millisecond) // ~300 DrainPace ticks

	fa := w.adapter.Load().a.(*fakeAdapter)
	fa.mu.Lock()
	calls := len(fa.calls)
	fa.mu.Unlock()
	if calls != 1 {
		t.Errorf("adapter called %d times; an un-removable delivered item must not be re-sent", calls)
	}
}

func newTestEngine(t *testing.T, dir string) (*Engine, *Registry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := NewRegistry()
	opts := testOpts(filepath.Join(dir, "spool"))
	return NewEngine(ctx, opts, reg, testLogger(t, dir), metrics.New()), reg
}

// Disabling a channel pauses it and keeps the spool; removing it entirely
// settles the backlog and cleans up. Without the second half those messages
// sit on disk forever, frozen at "queued" in the log.
func TestDisabledKeepsSpoolButRemovedIsPurged(t *testing.T) {
	dir := t.TempDir()
	e, _ := newTestEngine(t, dir)
	fa := &fakeAdapter{fn: func(notify.Notification) error { return adapter.Retryable("down") }}

	if err := e.SetChannels([]ChannelRuntime{{ID: "ch", Enabled: true, Adapter: fa}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(note("r_gone"), "ch", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the attempt fail and re-spool

	chDir := filepath.Join(dir, "spool", "ch")
	if err := e.SetChannels([]ChannelRuntime{{ID: "ch", Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chDir); err != nil {
		t.Fatal("disabling a channel must not purge its spool — that's a pause, not a purge")
	}

	if err := e.SetChannels(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chDir); !os.IsNotExist(err) {
		t.Error("a removed channel's spool dir should be cleaned up")
	}
	var settled bool
	for _, r := range readLog(t, dir) {
		if r.RequestID == "r_gone" && r.Outcome == "dropped: channel removed" {
			settled = true
		}
	}
	if !settled {
		t.Error("a removed channel's backlog must be settled in the log, not abandoned")
	}
}

// Disabling stops the worker that would otherwise apply the TTL, so without a
// sweep a parked channel's backlog outlives its TTL on disk: no terminal log
// line, and every 202 it holds stays unsettled forever.
func TestDisabledChannelBacklogStillExpires(t *testing.T) {
	dir := t.TempDir()
	e, _ := newTestEngine(t, dir)

	if err := e.SetChannels([]ChannelRuntime{{ID: "ch", Enabled: true, Adapter: &fakeAdapter{}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetChannels([]ChannelRuntime{{ID: "ch", Enabled: false}}); err != nil {
		t.Fatal(err)
	}

	sp := &spool{dir: filepath.Join(dir, "spool", "ch")}
	stale := time.Now().Add(-2 * time.Hour) // testOpts TTL is 1h
	if err := sp.put(&Item{Notification: note("r_parked"), Channel: "ch", EnqueuedAt: stale, NotBefore: stale}); err != nil {
		t.Fatal(err)
	}

	e.reapDisabled()

	if cnt, _ := sp.count(); cnt != 0 {
		t.Error("a parked channel's expired backlog must be dropped, not held past its TTL")
	}
	var settled bool
	for _, r := range readLog(t, dir) {
		if r.RequestID == "r_parked" && r.Outcome == "dropped: expired" {
			settled = true
		}
	}
	if !settled {
		t.Error("the drop needs its terminal log line — that's where the 202 gets settled")
	}
}

// A delivery that lands in the shutdown window has already unlinked its spool
// file by the time it writes its terminal line. Exiting without waiting loses
// that outcome from the log with nothing left on disk to re-settle at boot.
func TestShutdownWaitsForInFlightDelivery(t *testing.T) {
	dir := t.TempDir()
	e, _ := newTestEngine(t, dir)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fa := &fakeAdapter{fn: func(notify.Notification) error {
		started <- struct{}{}
		<-release
		return nil
	}}
	if err := e.SetChannels([]ChannelRuntime{{ID: "ch", Enabled: true, Adapter: fa}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(note("r_late"), "ch", false); err != nil {
		t.Fatal(err)
	}
	<-started // inside Send, spool file still on disk

	done := make(chan struct{})
	go func() { defer close(done); e.Shutdown(2 * time.Second) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a delivery was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown never returned")
	}
	var settled bool
	for _, r := range readLog(t, dir) {
		if r.RequestID == "r_late" && r.Outcome == "ok" {
			settled = true
		}
	}
	if !settled {
		t.Error("the delivery that landed in the shutdown window lost its terminal log line")
	}
}

// A channel id is joined straight into a spool path. `".."` aimed a worker's
// spool at the process working directory, whose config files the drain scan
// then read as zero-valued, long-expired items and deleted.
func TestEngineRefusesUnsafeChannelIDs(t *testing.T) {
	dir := t.TempDir()
	e, _ := newTestEngine(t, dir)
	for _, id := range []string{"..", ".", "a/b", ""} {
		if err := e.SetChannels([]ChannelRuntime{{ID: id, Enabled: true, Adapter: &fakeAdapter{}}}); err == nil {
			t.Errorf("channel id %q was accepted as a spool directory name", id)
		}
		if err := e.Enqueue(note("r_x"), id, false); err == nil {
			t.Errorf("enqueue to channel id %q must fail loudly", id)
		}
	}
}

// A spool that won't initialise (disk full, wrong ownership after a deploy)
// used to leave the channel dead until channels.toml next changed, 502-ing
// and dropping every notification in the meantime.
func TestEnqueueRetriesFailedSpoolInit(t *testing.T) {
	dir := t.TempDir()
	spoolRoot := filepath.Join(dir, "spool")
	if err := os.MkdirAll(spoolRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	// A regular file sitting where the channel's spool dir needs to be.
	blocker := filepath.Join(spoolRoot, "ch")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	e, _ := newTestEngine(t, dir)
	ch := ChannelRuntime{ID: "ch", Enabled: true, Adapter: &fakeAdapter{}}
	if err := e.SetChannels([]ChannelRuntime{ch}); err == nil {
		t.Fatal("a spool that won't initialise must be reported, not swallowed")
	}
	if err := e.Enqueue(note("r_1"), "ch", false); err == nil {
		t.Error("with no worker the enqueue must fail loudly")
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(note("r_2"), "ch", false); err != nil {
		t.Errorf("Enqueue must retry the spool init once the disk recovers: %v", err)
	}
}

// Removing a channel while its worker is live must not race: stop() used to
// read a cancel func the worker's own goroutine was still writing.
func TestSetChannelsStopsWorkerImmediately(t *testing.T) {
	dir := t.TempDir()
	e, _ := newTestEngine(t, dir)
	ch := ChannelRuntime{ID: "ch", Enabled: true, Adapter: &fakeAdapter{}}
	for i := 0; i < 20; i++ {
		if err := e.SetChannels([]ChannelRuntime{ch}); err != nil {
			t.Fatal(err)
		}
		if err := e.SetChannels(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Enqueue(note("r_x"), "ch", false); !errors.Is(err, ErrNoWorker) {
		t.Errorf("enqueue after removal = %v, want ErrNoWorker", err)
	}
}
