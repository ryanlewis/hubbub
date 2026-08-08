package dlog

import (
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// Reading the log back is the log's own business, not the HTTP layer's: the
// pairing of a `request` line with the `terminal` line that settles it later is
// a property of this file format, and a second package reconstructing it would
// be a second copy of the rule.

const (
	// initialTailBytes is how much of the log a read walks before deciding it
	// has not found enough. The log is append-only and rotated by whatever the
	// deployment set up, so it has no upper size this code controls — reading
	// all of it would let a month of history decide how long a poll takes.
	initialTailBytes = 2 << 20
	// maxTailBytes is the hard ceiling on that growth.
	//
	// The window has to be able to grow, because most lines in the file are not
	// deliveries: auth failures, admin edits and read audits all sit in the same
	// log, and a burst of refusals from an internet scanner could otherwise push
	// every real delivery out of a fixed window — answering a legitimate poll
	// with "nothing happened" on a hub that delivered a minute ago.
	maxTailBytes = 16 << 20
	// timeResolution is the precision this package reports timestamps at, and
	// therefore the precision Query.Since is compared at.
	//
	// The two must agree exactly or the documented cursor idiom — poll again
	// with the newest ts you hold — silently misbehaves: comparing a stored
	// nanosecond time against a cursor the caller round-tripped through a
	// coarser format leaves the boundary row strictly after it, so it comes back
	// on every poll for ever. Milliseconds are fine for a delivery log and keep
	// the emitted string a fixed width.
	timeResolution = time.Millisecond
)

// Delivery is one notification's outcome on one channel: the flattened view a
// recent-deliveries reader wants, rather than the two log lines it comes from.
// A fan-out to three channels is three of these.
type Delivery struct {
	// Time is when the notification was accepted — not when a retry finally
	// settled it. This is what a reader orders by: re-dating a message to the
	// moment a retry succeeded would shuffle it to the top of the list hours
	// after anyone cared.
	Time time.Time
	// Settled is when a later terminal line fixed the outcome, zero if none
	// did. Kept apart from Time so the two questions a caller has — when was
	// this news, and when did anything about it last change — do not have to
	// share one field.
	Settled   time.Time
	RequestID string
	CallerID  string
	Channel   string
	Title     string
	Priority  string
	Outcome   string
}

// Changed is when this delivery last changed, which is what Since filters on.
//
// Ordering by accept time and paging by accept time are not the same thing: a
// message accepted at 09:00 and settled `failed` at 11:00 would be invisible for
// ever to a caller whose cursor had already passed 09:00, so the outcome the
// whole endpoint exists to report would never arrive.
func (d Delivery) Changed() time.Time {
	if d.Settled.After(d.Time) {
		return d.Settled
	}
	return d.Time
}

// Query narrows what Recent returns. The zero value means "everything in the
// window".
type Query struct {
	// Limit caps how many are returned, newest first. Zero means no cap beyond
	// the window itself.
	Limit int
	// Since drops anything that has not changed since this instant. Zero means
	// no floor.
	Since time.Time
	// Channel, if set, keeps only that channel's deliveries.
	Channel string
}

// Page is one read of the log.
type Page struct {
	// Deliveries, newest first.
	Deliveries []Delivery
	// Truncated reports that more rows matched than Limit allowed, and that the
	// ones dropped were the *oldest* of the match. Without it a caller polling
	// with a cursor cannot tell a quiet period from a burst it only saw the end
	// of — and since its cursor then moves past the rest, what it missed would
	// never come back.
	Truncated bool
}

// Recent returns the newest deliveries in the log, newest first.
//
// Outcomes are reported as settled as the log knows them: a request line that
// recorded `queued` is upgraded to whatever its terminal line later said, since
// a 202 is a promise settled in the log rather than over HTTP. A delivery whose
// request line has already scrolled out of the window still appears — from its
// terminal line alone, without a title — because a real delivery reported as
// nothing at all is the worse failure.
func (l *Logger) Recent(q Query) (Page, error) {
	if l == nil {
		return Page{}, nil
	}
	// Grow the window until it holds what was asked for. Almost every read is
	// answered by the first pass; the escalation exists for a log whose recent
	// bytes are mostly not deliveries, where a fixed window would answer with
	// silence and look exactly like a hub that had delivered nothing.
	for window := int64(initialTailBytes); ; window *= 4 {
		buf, partial, err := l.tail(window)
		if err != nil {
			return Page{}, err
		}
		page := parseRecent(buf, partial, q)
		enough := q.Limit > 0 && len(page.Deliveries) >= q.Limit
		if enough || !partial || window >= maxTailBytes {
			return page, nil
		}
	}
}

// tail reads the last max bytes of the log, reporting whether the buffer starts
// part-way through a record.
func (l *Logger) tail(max int64) (buf []byte, partial bool, err error) {
	// The lock covers the size, not the read. Bytes below the recorded size are
	// already written and never rewritten — the log is append-only — so a
	// concurrent Append can only add beyond the range being read. Holding the
	// lock across the copy instead would put a multi-megabyte read in front of
	// every notification the hub is trying to log, including the terminal lines
	// that fsync under it.
	l.mu.Lock()
	st, err := l.f.Stat()
	l.mu.Unlock()
	if err != nil {
		return nil, false, err
	}

	size := st.Size()
	off := int64(0)
	if size > max {
		off = size - max
	}
	// One byte of overlap, so "did the window cut a line in half" is answered by
	// looking rather than assumed. Dropping the first line unconditionally
	// discards a whole record whenever the cut happens to land on a boundary.
	lead := int64(0)
	if off > 0 {
		off, lead = off-1, 1
	}

	buf = make([]byte, size-off)
	// ReadAt, never Read: this descriptor is opened O_APPEND for writing, and
	// seeking it would be a shared mutable position on a file another goroutine
	// is appending to.
	n, err := l.f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	buf = buf[:n]
	if lead > 0 && len(buf) > 0 {
		partial = buf[0] != '\n'
		buf = buf[1:]
	}
	return buf, partial, nil
}

// parseRecent folds raw log lines into deliveries.
func parseRecent(buf []byte, partial bool, q Query) Page {
	lines := strings.Split(string(buf), "\n")
	if partial && len(lines) > 0 {
		// The window started mid-record, so the first line is the tail end of
		// one whose beginning was cut off.
		lines = lines[1:]
	}

	var out []Delivery
	// index locates the delivery a line belongs to. Keyed on both ids because
	// one request fans out to several channels, each settling separately.
	index := map[[2]string]int{}
	// awaiting holds the rows built from a terminal line that arrived before
	// its own request line, and so are still missing their context. Kept apart
	// from index so a *repeated* request line — which the merge below would
	// otherwise read as one of these and stamp with a fabricated settle time —
	// is left alone instead.
	awaiting := map[[2]string]bool{}

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Record
		// A line that doesn't parse is skipped rather than fatal: a truncated
		// write or a hand-edit must not blank the whole view, and the lines
		// around it are still perfectly good records. The last line can also be
		// a write caught in flight, which the next read will see whole.
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}

		switch r.Kind {
		case "request":
			// Sorted so a fan-out lands in the same order on every read —
			// ranging a map would shuffle equal-timestamped rows between polls.
			// Descending here because page reverses the whole slice to put the
			// newest first, which leaves the fan-out reading a→z in the answer.
			for _, ch := range sortedChannels(r.Channels) {
				key := [2]string{r.RequestID, ch}
				if i, ok := index[key]; ok {
					if !awaiting[key] {
						// A request id names one accept, so a second line for
						// the same delivery is an anomaly rather than news.
						// Keeping the first is what makes two reads of the same
						// file agree.
						continue
					}
					// The terminal line got here first. That is not a corrupt
					// log: dispatch deregisters its waiters before handleNotify
					// writes the request line, so a delivery finishing in that
					// gap is settled in the log before the request it settles.
					// Fill in the context the terminal line had no room for and
					// keep the settled outcome, which is the newer truth.
					out[i].Settled, out[i].Time = out[i].Time, r.Time.Truncate(timeResolution)
					out[i].Title, out[i].Priority = r.Title, r.Priority
					if out[i].CallerID == "" {
						out[i].CallerID = r.CallerID
					}
					delete(awaiting, key)
					continue
				}
				index[key] = len(out)
				out = append(out, Delivery{
					Time:      r.Time.Truncate(timeResolution),
					RequestID: r.RequestID,
					CallerID:  r.CallerID,
					Channel:   ch,
					Title:     r.Title,
					Priority:  r.Priority,
					Outcome:   r.Channels[ch],
				})
			}
		case "terminal":
			key := [2]string{r.RequestID, r.Channel}
			if i, ok := index[key]; ok {
				out[i].Outcome = r.Outcome
				out[i].Settled = r.Time.Truncate(timeResolution)
				continue
			}
			// Indexed as well as appended, so a request line arriving after its
			// own terminal merges into this row instead of adding a second one
			// that reports the same delivery as still queued for ever.
			index[key], awaiting[key] = len(out), true
			out = append(out, Delivery{
				Time:      r.Time.Truncate(timeResolution),
				RequestID: r.RequestID,
				CallerID:  r.CallerID,
				Channel:   r.Channel,
				Outcome:   r.Outcome,
			})
		}
		// Everything else — auth_fail, admin, read, rate-capped requests —
		// describes something that was never a delivery to a channel.
	}

	return page(out, q)
}

// page applies the query's filters, orders newest first and cuts to the limit.
func page(in []Delivery, q Query) Page {
	// Rounded the same way the rows are, and downwards, so a cursor carrying
	// more precision than this package publishes errs towards returning a row
	// twice rather than dropping it.
	since := q.Since.Truncate(timeResolution)

	out := make([]Delivery, 0, len(in))
	for _, d := range slices.Backward(in) {
		if q.Channel != "" && d.Channel != q.Channel {
			continue
		}
		if !q.Since.IsZero() && !d.Changed().After(since) {
			continue
		}
		out = append(out, d)
	}
	// Walking backwards above already put the newest first for an append-only
	// log; the sort is for the clock, which can step back over a restart or an
	// NTP correction. Stable, so lines sharing a timestamp keep the reversed
	// file order rather than being shuffled.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })

	p := Page{Deliveries: out}
	if q.Limit > 0 && len(out) > q.Limit {
		p.Deliveries, p.Truncated = out[:q.Limit], true
	}
	return p
}

func sortedChannels(m map[string]string) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}
