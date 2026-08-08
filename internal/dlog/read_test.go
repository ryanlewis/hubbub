package dlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// at builds a timestamp n seconds into a fixed day, so the assertions below can
// talk about order without depending on when the suite runs.
func at(sec int) time.Time {
	return time.Date(2026, 8, 8, 9, 0, sec, 0, time.UTC)
}

func openTestLog(t *testing.T) *Logger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "delivery.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRecentFansOutOneItemPerChannel(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{
		Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron",
		Result: "partial", Title: "Backup failed", Priority: "high",
		Channels: map[string]string{"ntfy": "ok", "email": "queued"},
	})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 2 {
		t.Fatalf("got %d items, want one per channel: %+v", len(got), got)
	}
	// Sorted within the fan-out, so two polls of the same log agree.
	if got[0].Channel != "email" || got[1].Channel != "ntfy" {
		t.Errorf("channels = %q, %q; want a stable sorted pair", got[0].Channel, got[1].Channel)
	}
	for _, d := range got {
		if d.Title != "Backup failed" || d.CallerID != "cron" || d.RequestID != "r_1" || d.Priority != "high" {
			t.Errorf("item lost the request's context: %+v", d)
		}
	}
}

// A 202 is a promise settled in the log later, so the view has to show the
// settled outcome rather than the "queued" the caller was told at accept time.
func TestRecentUpgradesQueuedToItsTerminalOutcome(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{
		Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "t",
		Channels: map[string]string{"ntfy": "queued", "email": "queued"},
	})
	l.Append(Record{Time: at(90), Kind: "terminal", RequestID: "r_1", CallerID: "cron", Channel: "ntfy", Outcome: "ok"})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	byChannel := map[string]Delivery{}
	for _, d := range got {
		byChannel[d.Channel] = d
	}
	if len(got) != 2 {
		t.Fatalf("terminal line created a duplicate row instead of settling one: %+v", got)
	}
	if byChannel["ntfy"].Outcome != "ok" {
		t.Errorf("ntfy outcome = %q, want the settled ok", byChannel["ntfy"].Outcome)
	}
	if byChannel["email"].Outcome != "queued" {
		t.Errorf("email outcome = %q, want it left in flight", byChannel["email"].Outcome)
	}
	// The accept time survives the settle: a retry that succeeds two minutes
	// later is still news from when it was sent, not from when it landed.
	if !byChannel["ntfy"].Time.Equal(at(1)) {
		t.Errorf("settled item was re-dated to %v, want the accept time %v", byChannel["ntfy"].Time, at(1))
	}
}

// The request line can be older than the read window while its outcome is
// recent. Dropping the row would report a real delivery as nothing at all.
func TestRecentKeepsTerminalLinesWithNoRequestInTheWindow(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{Time: at(5), Kind: "terminal", RequestID: "r_old", CallerID: "cron", Channel: "ntfy", Outcome: "dropped: expired"})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 1 {
		t.Fatalf("got %d items, want the orphaned outcome kept: %+v", len(got), got)
	}
	if got[0].Outcome != "dropped: expired" || got[0].Channel != "ntfy" {
		t.Errorf("orphan = %+v", got[0])
	}
	if got[0].Title != "" {
		t.Errorf("title = %q, want empty — the line that carried it is gone", got[0].Title)
	}
}

// Auth failures, admin edits and rate-capped requests are in the same file and
// are not deliveries to a channel.
func TestRecentSkipsNonDeliveryLines(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{Time: at(1), Kind: "auth_fail", Peer: "10.0.0.1:1234", ClaimedIP: "203.0.113.9"})
	l.Append(Record{Time: at(2), Kind: "admin", Actor: "someone@example.com", Action: "caller_grants", Detail: "cron"})
	l.Append(Record{Time: at(3), Kind: "request", CallerID: "cron", Result: "rate_capped"})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 0 {
		t.Errorf("got %d items, want none: %+v", len(got), got)
	}
}

func TestRecentNewestFirstAndLimited(t *testing.T) {
	l := openTestLog(t)
	for i := 1; i <= 5; i++ {
		l.Append(Record{
			Time: at(i), Kind: "request", RequestID: "r_" + string(rune('0'+i)), CallerID: "cron",
			Title: "n" + string(rune('0'+i)), Channels: map[string]string{"ntfy": "ok"},
		})
	}

	page, err := l.Recent(Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 2 {
		t.Fatalf("limit ignored: %d items", len(got))
	}
	if got[0].Title != "n5" || got[1].Title != "n4" {
		t.Errorf("order = %q, %q; want the two newest, newest first", got[0].Title, got[1].Title)
	}
}

func TestRecentFilters(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "old",
		Channels: map[string]string{"ntfy": "ok", "email": "ok"}})
	l.Append(Record{Time: at(10), Kind: "request", RequestID: "r_2", CallerID: "cron", Title: "new",
		Channels: map[string]string{"ntfy": "ok"}})

	byChannelPage, err := l.Recent(Query{Channel: "email"})
	if err != nil {
		t.Fatal(err)
	}
	byChannel := byChannelPage.Deliveries
	if len(byChannel) != 1 || byChannel[0].Channel != "email" {
		t.Errorf("channel filter = %+v", byChannel)
	}

	// Strictly after, so polling with the newest ts already held returns only
	// what is genuinely new rather than repeating the boundary row.
	sincePage, err := l.Recent(Query{Since: at(1)})
	if err != nil {
		t.Fatal(err)
	}
	since := sincePage.Deliveries
	if len(since) != 1 || since[0].Title != "new" {
		t.Errorf("since filter = %+v", since)
	}
}

// A half-written or hand-mangled line must cost its own row and nothing else.
func TestRecentSkipsUnparseableLinesWithoutLosingTheRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "delivery.log")
	first := `{"ts":"2026-08-08T09:00:01Z","kind":"request","requestId":"r_1","callerId":"cron","title":"kept","channels":{"ntfy":"ok"}}`
	second := `{"ts":"2026-08-08T09:00:02Z","kind":"request","requestId":"r_2","callerId":"cron","title":"kept","channels":{"ntfy":"ok"}}`
	if err := os.WriteFile(path, []byte(first+"\n{\"ts\":\"2026-\n\n"+second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 2 {
		t.Fatalf("got %d items, want both good lines: %+v", len(got), got)
	}
}

// The window is a byte tail, so it can start mid-record. That fragment must not
// be parsed as if it were a whole line.
func TestTailDropsThePartialFirstLine(t *testing.T) {
	line := `{"ts":"2026-08-08T09:00:01Z","kind":"request","requestId":"r_1","callerId":"cron","title":"kept","channels":{"ntfy":"ok"}}`
	other := `{"ts":"2026-08-08T09:00:02Z","kind":"request","requestId":"r_2","callerId":"cron","title":"also kept","channels":{"ntfy":"ok"}}`
	buf := []byte(`aker":"cron","title":"cut in half"}` + "\n" + line + "\n")

	got := parseRecent(buf, true, Query{}).Deliveries
	if len(got) != 1 || got[0].Title != "kept" {
		t.Fatalf("partial first line was not discarded: %+v", got)
	}

	// The same bytes read from the top of the file are all real lines.
	got = parseRecent([]byte(line+"\n"+other+"\n"), false, Query{}).Deliveries
	if len(got) != 2 {
		t.Fatalf("whole-file read dropped a line: %+v", got)
	}
}

func TestTailIsBounded(t *testing.T) {
	l := openTestLog(t)
	// One oversized record, so the window certainly cuts inside the file.
	l.Append(Record{Time: at(1), Kind: "request", RequestID: "r_big", CallerID: "cron",
		Title: strings.Repeat("x", 4096), Channels: map[string]string{"ntfy": "ok"}})

	buf, partial, err := l.tail(512)
	if err != nil {
		t.Fatal(err)
	}
	if !partial {
		t.Error("tail did not report that it started mid-file")
	}
	// Exactly the window: tail also reads the byte before it, to see whether the
	// cut landed on a line boundary, and does not hand that one back.
	if len(buf) != 512 {
		t.Errorf("read %d bytes, want the window's 512", len(buf))
	}
}

// A window that happens to start exactly at a record boundary contains no
// fragment, and dropping its first line would silently lose a whole delivery.
func TestTailKeepsAWholeLineWhenTheCutLandsOnABoundary(t *testing.T) {
	l := openTestLog(t)
	first := Record{Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "first",
		Channels: map[string]string{"ntfy": "ok"}}
	l.Append(first)
	l.Append(Record{Time: at(2), Kind: "request", RequestID: "r_2", CallerID: "cron", Title: "second",
		Channels: map[string]string{"ntfy": "ok"}})

	// Size the window so it starts exactly where the second record does.
	st, err := l.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	firstLen, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	buf, partial, err := l.tail(st.Size() - int64(len(firstLen)) - 1)
	if err != nil {
		t.Fatal(err)
	}
	if partial {
		t.Fatal("a window starting on a boundary was reported as cutting a line in half")
	}
	got := parseRecent(buf, partial, Query{}).Deliveries
	if len(got) != 1 || got[0].Title != "second" {
		t.Fatalf("boundary-aligned window = %+v, want the whole second record", got)
	}
}

// The window has to grow past a burst of non-delivery lines. A scanner probing
// an internet-facing hub writes refusal records at request rate, and a fixed
// window would let it push every real delivery out of view — answering a
// legitimate poll with silence from a hub that delivered a minute ago.
func TestRecentWidensThePastNonDeliveryLines(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "buried",
		Channels: map[string]string{"ntfy": "ok"}})
	// Comfortably more than the initial window, all of it noise.
	noise := Record{Kind: "auth_fail", Peer: "10.0.0.1:1234", ClaimedIP: strings.Repeat("9", 200)}
	for range 12000 {
		l.Append(noise)
	}

	page, err := l.Recent(Query{Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 1 || page.Deliveries[0].Title != "buried" {
		t.Fatalf("got %+v, want the delivery the refusals buried", page.Deliveries)
	}
}

// dispatch deregisters its waiters before handleNotify writes the request line,
// so a delivery that finishes in that gap settles in the log *before* the
// request it settles. Both lines describe one delivery and must fold into one
// row — the alternative is a permanent duplicate, one half stuck at "queued".
func TestRecentMergesATerminalWrittenBeforeItsRequest(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{Time: at(3), Kind: "terminal", RequestID: "r_1", CallerID: "cron", Channel: "ntfy", Outcome: "ok"})
	l.Append(Record{Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "Backup failed",
		Priority: "high", Channels: map[string]string{"ntfy": "queued"}})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 1 {
		t.Fatalf("got %d rows for one delivery: %+v", len(got), got)
	}
	if got[0].Outcome != "ok" {
		t.Errorf("outcome = %q, want the settled outcome to win over the accept line's queued", got[0].Outcome)
	}
	if got[0].Title != "Backup failed" || got[0].Priority != "high" {
		t.Errorf("row never picked up the request's context: %+v", got[0])
	}
	if !got[0].Time.Equal(at(1)) {
		t.Errorf("ts = %v, want the accept time %v", got[0].Time, at(1))
	}
	if !got[0].Settled.Equal(at(3)) {
		t.Errorf("settled = %v, want the terminal line's time %v", got[0].Settled, at(3))
	}
}

// Timestamps are published at the precision `since` is compared at, so a cursor
// a caller round-tripped through the API excludes exactly the row it names.
func TestSinceRoundTripsAtTheEmittedPrecision(t *testing.T) {
	l := openTestLog(t)
	// Sub-millisecond detail is what a real clock gives Append.
	odd := time.Date(2026, 8, 8, 9, 0, 1, 123456789, time.UTC)
	l.Append(Record{Time: odd, Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "one",
		Channels: map[string]string{"ntfy": "ok"}})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.Deliveries[0].Time
	if cursor.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("published time %v carries precision finer than it is compared at", cursor)
	}

	page, err = l.Recent(Query{Since: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 0 {
		t.Errorf("polling with the row's own ts returned it again: %+v", page.Deliveries)
	}
}

// Ordering by accept time and paging by accept time are different questions: an
// outcome settled hours later must still reach a caller whose cursor has long
// passed the accept time, or the endpoint never reports the failure it exists
// to report.
func TestSinceFollowsLateSettledOutcomes(t *testing.T) {
	l := openTestLog(t)
	l.Append(Record{Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "Backup failed",
		Channels: map[string]string{"email": "queued"}})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.Deliveries[0].Time
	if page.Deliveries[0].Outcome != "queued" {
		t.Fatalf("first poll = %+v, want it in flight", page.Deliveries[0])
	}

	// Two hours later the outbox gives up.
	l.Append(Record{Time: at(1).Add(2 * time.Hour), Kind: "terminal", RequestID: "r_1", CallerID: "cron",
		Channel: "email", Outcome: "failed: smtp: 550 mailbox unavailable"})

	page, err = l.Recent(Query{Since: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 1 {
		t.Fatalf("the settled failure never reached a cursor-following caller: %+v", page.Deliveries)
	}
	if page.Deliveries[0].Outcome != "failed: smtp: 550 mailbox unavailable" {
		t.Errorf("outcome = %q", page.Deliveries[0].Outcome)
	}
	// Still displayed where it happened, not where it settled.
	if !page.Deliveries[0].Time.Equal(at(1)) {
		t.Errorf("ts = %v, want the accept time", page.Deliveries[0].Time)
	}
}

// The rows a limit drops are the oldest of the match, and a cursor-following
// caller's next poll moves past them for good — so it has to be told.
func TestPageReportsTruncation(t *testing.T) {
	l := openTestLog(t)
	for i := 1; i <= 4; i++ {
		l.Append(Record{Time: at(i), Kind: "request", RequestID: "r_" + string(rune('0'+i)), CallerID: "cron",
			Title: "n", Channels: map[string]string{"ntfy": "ok"}})
	}

	page, err := l.Recent(Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated {
		t.Error("cut two rows off the answer without saying so")
	}

	page, err = l.Recent(Query{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if page.Truncated {
		t.Error("reported truncation on a complete answer")
	}
}

// Reading goes through the same descriptor the hub appends to, which is what
// makes a rotation-by-rename invisible to a poller: the process keeps writing
// to the renamed inode, and a reader following the path would answer from an
// empty new file instead.
func TestRecentReadsThroughTheOpenDescriptorAcrossARename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "delivery.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Append(Record{Time: at(1), Kind: "request", RequestID: "r_1", CallerID: "cron", Title: "before",
		Channels: map[string]string{"ntfy": "ok"}})
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	l.Append(Record{Time: at(2), Kind: "request", RequestID: "r_2", CallerID: "cron", Title: "after",
		Channels: map[string]string{"ntfy": "ok"}})

	page, err := l.Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 2 {
		t.Fatalf("got %d items, want both sides of the rotation: %+v", len(got), got)
	}
	if got[0].Title != "after" {
		t.Errorf("newest = %q, want the line written after the rename", got[0].Title)
	}
}

func TestRecentOnAnEmptyLog(t *testing.T) {
	page, err := openTestLog(t).Recent(Query{})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Deliveries
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}
