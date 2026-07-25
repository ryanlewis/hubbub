// Package httpapi carries the caller-facing JSON API (public port) and the
// ops surface (ops port): auth, the global rate cap, ingest validation, and
// the wait-window response contract over the outbox.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/metrics"
	"github.com/ryanlewis/hubbub/internal/notify"
	"github.com/ryanlewis/hubbub/internal/outbox"
)

const (
	maxBodyBytes = 64 << 10 // reject before buffering
	// maxClaimedIPBytes bounds the untrusted X-Forwarded-For copied into the
	// delivery log. The auth-failure line is written before the rate cap
	// applies, so an unauthenticated caller would otherwise choose the size of
	// every log line it provokes and fill the disk the spool lives on.
	maxClaimedIPBytes = 256
	Version           = "0.1.0"
)

type Server struct {
	Store   *config.Store
	Engine  *outbox.Engine
	Reg     *outbox.Registry
	Log     *dlog.Logger
	Metrics *metrics.Metrics
	Rate    *RateLimiter
	Window  time.Duration
}

// PublicMux serves the caller-facing API.
func (s *Server) PublicMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/notify", s.handleNotify)
	mux.HandleFunc("GET /health", s.handleHealth)
	// Unauthenticated on purpose: the spec exposes shape, not secrets, so an
	// agent can be pointed at the base URL and discover the contract before it
	// has been issued a key.
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	// Same reasoning, for the two audiences that arrive without a key: a person
	// who opened the base URL in a browser, and an agent that was handed the
	// host and nothing else.
	//
	// Registered on the exact root — "/{$}" — never a bare "/", which is a
	// subtree pattern: that would answer every unrouted path with the landing
	// page, turning a caller's typo'd URL into 200 OK full of HTML instead of
	// the 404 it needs to see.
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /index.html", handleIndex)
	mux.HandleFunc("GET /llms.txt", handleLLMsTxt)
	mux.HandleFunc("GET /favicon.svg", handleFavicon)
	// The browsable reference. Rendered from the same resolved spec, so it can
	// only ever show what /openapi.json already says.
	mux.HandleFunc("GET /docs", s.handleDocs)
	return mux
}

// OpsMux serves /metrics, /health and the test CTA. Exposure is decided by
// which port it binds — outside the proxy range means internet-unreachable —
// so it carries no auth machinery of its own.
func (s *Server) OpsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, s.Metrics.Render())
	})
	mux.HandleFunc("POST /test/{channel}", s.handleTest)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
		"uptime":  s.Metrics.Uptime().Round(time.Second).String(),
	})
}

type notifyRequest struct {
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Priority string   `json:"priority"`
	Tags     []string `json:"tags"`
	// Pointer so an omitted list is distinguishable from an explicit empty
	// one: the first means "the key's full permission set", the second is a
	// caller that computed no targets and must not fan out to everything.
	Channels *[]string `json:"channels"`
}

type notifyResponse struct {
	Result    string            `json:"result"`
	RequestID string            `json:"requestId"`
	Channels  map[string]string `json:"channels"`
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	// 1. Authenticate.
	caller, ok := s.authenticate(r)
	if !ok {
		s.Metrics.AuthFailure()
		s.Log.Append(dlog.Record{
			Kind: "auth_fail",
			Peer: r.RemoteAddr,
			// Recorded as claimed, never trusted — and never at the caller's
			// chosen length.
			ClaimedIP: clip(r.Header.Get("X-Forwarded-For"), maxClaimedIPBytes),
		})
		writeError(w, http.StatusUnauthorized, "missing or unknown bearer key")
		return
	}

	// 2. Global rate cap: after auth, before enqueue. Capped requests are
	// logged — they're the runaway evidence.
	if allowed, retryAfter := s.Rate.Allow(time.Now()); !allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds()+0.5)))
		s.Metrics.Request("rate_capped")
		s.Log.Append(dlog.Record{
			Kind:     "request",
			CallerID: caller.ID,
			Result:   "rate_capped",
		})
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"result": "rate_capped"})
		return
	}

	// 3. Parse + validate, strictly: unknown fields tell a caller its schema
	// is stale rather than silently dropping data.
	req, err := decodeNotify(w, r)
	if err != nil {
		s.Metrics.Request("rejected")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prio, err := notify.ParsePriority(req.Priority)
	if err != nil {
		s.Metrics.Request("rejected")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 4. Channel selection: the key's permission list is the delivery set;
	// an explicit channels list narrows, never widens — naming a channel the
	// key lacks is a 403, not a silent intersection.
	targets := caller.Channels
	if req.Channels != nil {
		// An explicit `"channels": []` is the one input that could widen:
		// treating it as "field absent" fans a caller that computed no targets
		// out to every channel its key permits. Reject it instead.
		if len(*req.Channels) == 0 {
			s.Metrics.Request("rejected")
			writeError(w, http.StatusBadRequest, "channels was empty: omit the field to use the key's full permission list")
			return
		}
		for _, ch := range *req.Channels {
			if !caller.Permitted(ch) {
				s.Metrics.Request("forbidden")
				writeError(w, http.StatusForbidden, fmt.Sprintf("channel %q not permitted for this key", ch))
				return
			}
		}
		targets = *req.Channels
	}
	if len(targets) == 0 {
		s.Metrics.Request("rejected")
		writeError(w, http.StatusBadRequest, "key has no permitted channels")
		return
	}

	n := notify.Notification{
		Title:     notify.SanitizeTitle(req.Title),
		Message:   notify.SanitizeMessage(req.Message),
		Priority:  prio,
		Tags:      notify.SanitizeTags(req.Tags),
		CallerID:  caller.ID,
		RequestID: notify.NewRequestID(),
		CreatedAt: time.Now().UTC(),
	}
	if err := n.Validate(); err != nil {
		s.Metrics.Request("rejected")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 5. Enqueue everything, wait out the response window, report what's
	// known: finished attempts concretely, in-flight as queued.
	results := s.dispatch(n, targets, false)

	status, result := statusFor(results)
	s.Metrics.Request(result)
	s.Log.Append(dlog.Record{
		Kind:      "request",
		RequestID: n.RequestID,
		CallerID:  caller.ID,
		Result:    result,
		Channels:  results,
		Title:     n.Title,
		MsgBytes:  len(n.Message),
		Priority:  string(n.Priority),
	})
	writeJSON(w, status, notifyResponse{Result: result, RequestID: n.RequestID, Channels: results})
}

// dispatch enqueues n for each target and collects outcomes until the
// response window closes. Channels that miss the window report "queued".
func (s *Server) dispatch(n notify.Notification, targets []string, test bool) map[string]string {
	results := make(map[string]string, len(targets))
	chs := s.Store.Channels()

	// De-duplicate targets. A repeated id — in the request's channels array or
	// in a key's channels list — would otherwise be spooled and delivered
	// twice, and since the registry keys waiters on request+channel, only one
	// of the two outcomes can resolve: the handler would then sit out its
	// whole response window waiting for an outcome that can never arrive.
	var waiting []string
	seen := make(map[string]bool, len(targets))
	for _, id := range targets {
		if seen[id] {
			continue
		}
		seen[id] = true
		ch, exists := chs.Get(id)
		if !exists || !ch.Enabled {
			// Permitted-but-disabled (or typo'd) is a visible nag, never a
			// silent skip — and not a 403, which is for permission violations.
			results[id] = "disabled"
			continue
		}
		waiting = append(waiting, id)
	}

	if len(waiting) == 0 {
		return results
	}

	// Register the waiter before enqueueing so a fast delivery can't slip
	// through before we're listening.
	outcomes := s.Reg.Add(n.RequestID, waiting)
	enqueued := make([]string, 0, len(waiting))
	for _, id := range waiting {
		if err := s.Engine.Enqueue(n, id, test); err != nil {
			s.Reg.Cancel(n.RequestID, []string{id})
			results[id] = "failed: " + err.Error()
			continue
		}
		enqueued = append(enqueued, id)
	}

	pending := len(enqueued)
	timer := time.NewTimer(s.Window)
	defer timer.Stop()
	for pending > 0 {
		select {
		case o := <-outcomes:
			results[o.Channel] = o.Status
			pending--
		case <-timer.C:
			// Window closed: deregister the rest; whatever resolved in the
			// race gets drained below, the rest settle in the log.
			s.Reg.Cancel(n.RequestID, enqueued)
			for {
				select {
				case o := <-outcomes:
					results[o.Channel] = o.Status
				default:
					for _, id := range enqueued {
						if _, done := results[id]; !done {
							results[id] = "queued"
						}
					}
					return results
				}
			}
		}
	}
	return results
}

// statusFor maps per-channel results onto the response contract:
// 200 all delivered · 202 all queued (promise, not receipt) · 502 all failed
// permanently · 207 anything mixed.
func statusFor(results map[string]string) (int, string) {
	var okCnt, queuedCnt, failedCnt, disabledCnt int
	for _, v := range results {
		switch {
		case v == "ok":
			okCnt++
		case v == "queued":
			queuedCnt++
		case v == "disabled":
			disabledCnt++
		default: // failed: …, dropped: …
			failedCnt++
		}
	}
	total := len(results)
	switch {
	case okCnt == total:
		return http.StatusOK, "delivered"
	case queuedCnt == total:
		return http.StatusAccepted, "queued"
	case failedCnt == total:
		return http.StatusBadGateway, "failed"
	case disabledCnt == total:
		// Nothing was attempted, so nothing failed *permanently* — 502 is
		// defined as the latter. A key left permitted a disabled channel nags
		// as 207 until the config inconsistency is fixed; 502 would instead
		// invite generic 5xx retry machinery to hammer a request that cannot
		// succeed until a human edits channels.toml.
		return http.StatusMultiStatus, "partial"
	default:
		return http.StatusMultiStatus, "partial"
	}
}

// handleTest is the per-channel test-send CTA: fires a canned notification
// through one adapter end to end. Ops port only; bypasses the rate cap
// (testing must work mid-throttle) and jumps the spool queue.
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	chs := s.Store.Channels()
	ch, exists := chs.Get(id)
	if !exists {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown channel %q (known: %s)", id, strings.Join(chs.IDs(), ", ")))
		return
	}
	if !ch.Enabled {
		writeError(w, http.StatusConflict, fmt.Sprintf("channel %q is disabled", id))
		return
	}

	n := notify.Notification{
		Title:     "hubbub test",
		Message:   fmt.Sprintf("test send to channel %q at %s", id, time.Now().UTC().Format(time.RFC3339)),
		Priority:  notify.PriorityDefault,
		CallerID:  "test-cta",
		RequestID: notify.NewRequestID(),
		CreatedAt: time.Now().UTC(),
	}
	results := s.dispatch(n, []string{id}, true)
	status, result := statusFor(results)
	s.Log.Append(dlog.Record{
		Kind:      "request",
		RequestID: n.RequestID,
		CallerID:  n.CallerID,
		Result:    result,
		Channels:  results,
		Title:     n.Title,
	})
	writeJSON(w, status, notifyResponse{Result: result, RequestID: n.RequestID, Channels: results})
}

func (s *Server) authenticate(r *http.Request) (*config.Caller, bool) {
	auth := r.Header.Get("Authorization")
	// RFC 7235 §2.1 makes the auth-scheme token case-insensitive, and clients
	// and proxies do emit "bearer". Matching it case-sensitively turns a
	// casing difference into a 401 plus an auth_fail line — making a client
	// bug indistinguishable in the audit trail from someone guessing keys.
	const prefix = "Bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return nil, false
	}
	return s.Store.Keyring().Lookup(strings.TrimSpace(auth[len(prefix):]))
}

// decodeNotify needs w: http.MaxBytesReader signals an over-length body back
// through the ResponseWriter, and only that signal marks the connection for
// close. Passed nil, the type assertion inside net/http fails silently and the
// server goes on draining the oversized body after the handler has answered.
func decodeNotify(w http.ResponseWriter, r *http.Request) (*notifyRequest, error) {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req notifyRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
		}
		return nil, fmt.Errorf("invalid request: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid request: trailing data after JSON object")
	}
	return &req, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// clip truncates s to at most max bytes, on a rune boundary, marking the cut.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}
