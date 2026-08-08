package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
)

// GET /v1/recent is the delivery log as a machine-readable page: what came in,
// where it went and how it ended up. Built for a dashboard widget polling every
// few minutes, so the shape is flat and fixed rather than clever — one row per
// notification per channel, newest first, every field always present.

const (
	// recentDefaultLimit is a screenful. Reading further is opting in.
	recentDefaultLimit = 50
	// recentMaxLimit caps what one poll can ask for. The endpoint is a recent
	// view, not an export: a reader wanting the whole log has SSH and a file.
	recentMaxLimit = 500
)

// recentParams is the accepted query string, in full.
//
// Named here rather than only in the parser because the served spec is pinned
// to it by openapi_test.go — a knob added in code without being written down is
// a knob no caller can discover.
var recentParams = []string{"limit", "since", "channel"}

// recentTimeFormat is RFC3339 fixed at millisecond precision — the resolution
// dlog compares `since` at. A timestamp printed coarser than it is compared
// cannot be used as a cursor: the row it names stays strictly newer than
// itself and comes back on every poll.
const recentTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// recentResponse keeps `items` an array and `count` its length even when
// nothing matched, so a consumer can index the shape unconditionally. Machine
// callers branch on fields being there, not on their being absent.
type recentResponse struct {
	Items []recentItem `json:"items"`
	Count int          `json:"count"`
	// Truncated says the limit cut older matching rows off this answer. A
	// cursor-following caller has to know: the rows dropped are the oldest of
	// the match, and its next `since` moves past them for good.
	Truncated bool `json:"truncated"`
}

// recentItem is one delivery. No field is omitempty: a title that never made it
// into the log (a delivery whose request line has scrolled out of the window)
// reports as an empty string rather than a missing key, so the row still fits
// the same template as every other.
type recentItem struct {
	Time      string `json:"ts"`
	SettledAt string `json:"settledAt"`
	RequestID string `json:"requestId"`
	Caller    string `json:"caller"`
	Channel   string `json:"channel"`
	Title     string `json:"title"`
	Priority  string `json:"priority"`
	Outcome   string `json:"outcome"`
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	// Set before anything can answer, refusals included. This body is every
	// caller's titles and caller ids — the same content the dashboard sets
	// `no-store` for, and on the admin-identity path there is no Authorization
	// header to make a cache treat it as private on its own.
	w.Header().Set("Cache-Control", "no-store, private")

	caller, ok := s.authorizeRecent(w, r)
	if !ok {
		return
	}

	q, err := parseRecentQuery(r.URL.Query())
	if err != nil {
		s.Metrics.RecentRead("rejected")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Deliberately outside the global rate cap, which exists to bound how much
	// a runaway caller can deliver — a read spends no upstream quota and wakes
	// nobody's phone. A poller sharing the send budget would also mean a
	// dashboard tab could rate-cap the alerts it exists to display.
	page, err := s.Log.Recent(q)
	if err != nil {
		s.Metrics.RecentRead("error")
		writeError(w, http.StatusInternalServerError, "delivery log unavailable")
		return
	}

	items := make([]recentItem, 0, len(page.Deliveries))
	for _, d := range page.Deliveries {
		items = append(items, recentItem{
			Time:      d.Time.UTC().Format(recentTimeFormat),
			SettledAt: formatSettled(d.Settled),
			RequestID: d.RequestID,
			Caller:    d.CallerID,
			Channel:   d.Channel,
			Title:     d.Title,
			Priority:  d.Priority,
			Outcome:   d.Outcome,
		})
	}

	s.Metrics.RecentRead("ok")
	s.auditRead(r, caller, q, len(items))
	writeJSON(w, http.StatusOK, recentResponse{Items: items, Count: len(items), Truncated: page.Truncated})
}

// formatSettled renders the settle time, empty where nothing settled the row
// after it was accepted. Empty rather than a repeat of `ts`, so "this outcome
// was decided later" stays distinguishable from "this was already final".
func formatSettled(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(recentTimeFormat)
}

// auditRead records who read the log.
//
// The dashboard already writes a line for every config change, on the grounds
// that a change nobody can attribute later is barely better than no change
// control at all. Reading every caller's notification content is the same class
// of privilege, and leaving only refusals in the log would mean the successful
// reads — the ones that actually disclosed something — are the invisible ones.
// The query is recorded, never the rows it returned.
func (s *Server) auditRead(r *http.Request, caller *config.Caller, q dlog.Query, returned int) {
	rec := dlog.Record{
		Kind:   "read",
		Action: "recent",
		Detail: fmt.Sprintf("limit=%d since=%q channel=%q returned=%d", q.Limit, formatSettled(q.Since), q.Channel, returned),
	}
	if caller != nil {
		rec.CallerID = caller.ID
	} else if s.Admin != nil {
		// The other credential: an operator, recorded the way admin edits are.
		id, _, _ := s.Admin.Guard.Identity(r)
		rec.Actor = id.Email
	}
	s.Log.Append(rec)
}

// authorizeRecent gates the log behind an admin credential, answering the
// request itself when it refuses. The caller it returns is the key that was
// presented, nil for the admin-identity path.
//
// Reading is a strictly higher privilege than sending: the log holds every
// caller's titles, caller ids and outcomes, so a key issued to a cron job — or
// a low-trust token that leaks — must not become a window onto everyone else's
// notifications. Two credentials qualify, and holding any valid key is not one
// of them.
func (s *Server) authorizeRecent(w http.ResponseWriter, r *http.Request) (*config.Caller, bool) {
	// A bearer key is answered on its own merits whenever one is presented, so
	// a machine that sent the wrong key is told so plainly instead of quietly
	// succeeding on some other credential the request happened to carry.
	if r.Header.Get("Authorization") != "" {
		caller, ok := s.authenticate(r)
		if !ok {
			// A bad credential, counted where credential-guessing is watched.
			s.Metrics.AuthFailure()
			s.denyRecent(w, r, "", "unauthorized", http.StatusUnauthorized, "missing or unknown bearer key")
			return nil, false
		}
		if !caller.Recent {
			// A good credential used outside its grant. Deliberately *not*
			// counted as an auth failure: handleNotify keeps permission
			// refusals out of that counter for the same reason, since a
			// dashboard configured with a send-only key would otherwise read as
			// someone guessing keys.
			s.denyRecent(w, r, caller.ID, "forbidden", http.StatusForbidden,
				"this key may send but not read the delivery log; grant it with recent = true in keys.toml")
			return nil, false
		}
		return caller, true
	}

	// An allowlisted operator identity, so the log is readable from the browser
	// already trusted to edit keys and channel credentials. Same guard, same
	// listener and therefore the same assumption as /admin: the identity header
	// is only meaningful because the deployment's proxy asserts it. A hub with
	// no [admin] block has no such identity, and falls through to the 401.
	if s.Admin != nil {
		if _, _, permitted := s.Admin.Guard.Identity(r); permitted {
			return nil, true
		}
	}
	s.Metrics.AuthFailure()
	s.denyRecent(w, r, "", "unauthorized", http.StatusUnauthorized, "missing or unknown bearer key")
	return nil, false
}

// denyRecent refuses a read and leaves the evidence.
//
// A refused read is logged for the same reason a refused send is: it is what a
// leaked or guessed credential looks like from inside the hub, and the delivery
// log is the file that survives a restart. The claimed forwarding header is
// clipped exactly as it is on the notify path — an unauthenticated caller must
// not get to choose the size of the lines it provokes.
func (s *Server) denyRecent(w http.ResponseWriter, r *http.Request, callerID, outcome string, status int, msg string) {
	s.Metrics.RecentRead(outcome)
	s.Log.Append(dlog.Record{
		Kind:      "auth_fail",
		CallerID:  callerID,
		Peer:      r.RemoteAddr,
		ClaimedIP: clip(r.Header.Get("X-Forwarded-For"), maxClaimedIPBytes),
		Detail:    "recent: " + msg,
	})
	writeError(w, status, msg)
}

// parseRecentQuery reads the query string strictly.
//
// Unknown parameters are rejected rather than ignored, mirroring the notify
// decoder's DisallowUnknownFields: a misspelled `chanel=` that silently returns
// every channel is a filter a caller believes is applied. The error names what
// is accepted, since there is no request body whose 400 would explain itself.
func parseRecentQuery(values url.Values) (dlog.Query, error) {
	q := dlog.Query{Limit: recentDefaultLimit}

	for name := range values {
		if !slices.Contains(recentParams, name) {
			return q, fmt.Errorf("unknown query parameter %q (accepted: %s)", name, strings.Join(recentParams, ", "))
		}
	}

	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return q, fmt.Errorf("limit must be a whole number, got %q", raw)
		}
		// Clamping a too-large limit would answer a request for 5000 with 500
		// and no way to tell that from the log holding only that many. Both
		// ends are errors so the caller knows exactly what it got.
		if n < 1 || n > recentMaxLimit {
			return q, fmt.Errorf("limit must be between 1 and %d, got %d", recentMaxLimit, n)
		}
		q.Limit = n
	}

	if raw := values.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, fmt.Errorf("since must be an RFC3339 timestamp such as 2026-08-08T09:30:00Z, got %q", raw)
		}
		q.Since = t
	}

	// Not validated against configured channels: an id that no longer exists is
	// a legitimate thing to ask about — its deliveries are still in the log,
	// and a channel deleted this morning is exactly what someone would go
	// looking for.
	q.Channel = values.Get("channel")

	return q, nil
}
