package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/metrics"
	"github.com/ryanlewis/hubbub/internal/outbox"
)

const devKey = "nh_test_key_0123456789"

// newTestServer wires the full vertical — Store → Engine → worker → ntfy
// adapter → the given upstream — with a short response window.
func newTestServer(t *testing.T, upstreamURL string, channelsTOML string) *Server {
	t.Helper()
	dir := t.TempDir()

	if channelsTOML == "" {
		channelsTOML = "[ntfy]\ntype = \"ntfy\"\nserver = \"" + upstreamURL + "\"\ntopic = \"tst\"\n"
	}
	keysPath := filepath.Join(dir, "keys.toml")
	chansPath := filepath.Join(dir, "channels.toml")
	os.WriteFile(keysPath, []byte("[dev]\nkey = \""+devKey+"\"\nchannels = [\"ntfy\"]\n"), 0o600)
	os.WriteFile(chansPath, []byte(channelsTOML), 0o600)

	store, err := config.NewStore(&config.Config{KeysFile: keysPath, ChannelsFile: chansPath})
	if err != nil {
		t.Fatal(err)
	}
	logger, err := dlog.Open(filepath.Join(dir, "delivery.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })

	m := metrics.New()
	reg := outbox.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	engine := outbox.NewEngine(ctx, outbox.Options{
		SpoolDir:       filepath.Join(dir, "spool"),
		TTL:            time.Hour,
		CapPerChannel:  10,
		DrainPace:      time.Millisecond,
		AttemptTimeout: time.Second,
	}, reg, logger, m)
	var rts []outbox.ChannelRuntime
	for _, ch := range store.Channels().All() {
		rts = append(rts, outbox.ChannelRuntime{ID: ch.ID, Enabled: ch.Enabled, Adapter: ch.Adapter})
	}
	if err := engine.SetChannels(rts); err != nil {
		t.Fatal(err)
	}

	return &Server{
		Store:   store,
		Engine:  engine,
		Reg:     reg,
		Log:     logger,
		Metrics: m,
		Rate:    NewRateLimiter(100, time.Hour),
		Window:  500 * time.Millisecond,
	}
}

func post(t *testing.T, mux http.Handler, path, key, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestNotifyDelivered(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"hi","message":"there"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if body["result"] != "delivered" {
		t.Errorf("result = %v", body["result"])
	}
	chans := body["channels"].(map[string]any)
	if chans["ntfy"] != "ok" {
		t.Errorf("channels = %v", chans)
	}
	if !strings.HasPrefix(body["requestId"].(string), "r_") {
		t.Errorf("requestId = %v", body["requestId"])
	}
}

func TestNotifyQueuedWhenChannelDown(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"hi","message":"m"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (all queued)", rec.Code)
	}
	if body["result"] != "queued" {
		t.Errorf("result = %v", body["result"])
	}
}

func TestNotifyTotalPermanentFailureIs502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // permanent
	}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"hi","message":"m"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if body["result"] != "failed" {
		t.Errorf("result = %v", body["result"])
	}
}

func TestNotifyAuthRequired(t *testing.T) {
	s := newTestServer(t, "http://unused.invalid", "")
	rec, _ := post(t, s.PublicMux(), "/v1/notify", "", `{"title":"a","message":"b"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
	rec, _ = post(t, s.PublicMux(), "/v1/notify", "nh_wrong_key_00000000", `{"title":"a","message":"b"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad key: %d", rec.Code)
	}
}

func TestNotifyRejectsUnknownFields(t *testing.T) {
	s := newTestServer(t, "http://unused.invalid", "")
	rec, _ := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b","priorty":"high"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d, want 400 (stale schema must be loud)", rec.Code)
	}
}

func TestNotifyForbiddenChannel(t *testing.T) {
	s := newTestServer(t, "http://unused.invalid", "")
	rec, _ := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b","channels":["discord"]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unpermitted channel: %d, want 403 (never silently intersected)", rec.Code)
	}
}

// A disabled channel was never attempted, so it is not a permanent failure:
// 502 is defined as "every selected channel failed permanently". Answering 502
// invites generic 5xx retry machinery to hammer a request that cannot succeed
// until channels.toml is edited; 207 keeps it the visible config nag the
// design calls for.
func TestNotifyDisabledChannelIsVisibleNag(t *testing.T) {
	chans := "[ntfy]\ntype = \"ntfy\"\nserver = \"http://unused.invalid\"\ntopic = \"t\"\nenabled = false\n"
	s := newTestServer(t, "", chans)
	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b"}`)
	if rec.Code != http.StatusMultiStatus {
		t.Errorf("all-disabled: %d, want 207 (a config nag, not a gateway failure)", rec.Code)
	}
	if body["result"] != "partial" {
		t.Errorf("result = %v, want partial", body["result"])
	}
	if body["channels"].(map[string]any)["ntfy"] != "disabled" {
		t.Errorf("per-channel result = %v, want disabled (never a silent skip)", body["channels"])
	}
}

// The design's rule is "optional channels narrows, never widens". An explicit
// empty list was the one input that widened: it read as "field absent" and
// fanned a caller that had computed no targets out to everything its key
// permits.
func TestNotifyRejectsAnExplicitlyEmptyChannelList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an empty channel selection must not deliver anything")
	}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, _ := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b","channels":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty channels: %d, want 400 (never a silent widen to the full permission set)", rec.Code)
	}
}

// `null` is the same shape of hazard as `[]`, and a *[]string could not tell it
// from an omitted field: encoding/json sets the pointer to nil for both, so a
// caller whose target list came back null fanned out to everything its key
// permits.
func TestNotifyRejectsANullChannelList(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a null channel selection must not deliver anything")
	}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b","channels":null}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("null channels: %d, want 400 (never a silent widen to the full permission set)", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "null") {
		t.Errorf("error = %q, want it to name what was wrong", msg)
	}
}

// dec.More() is not a trailing-data check: it reports whether another *value*
// follows, and a stray closing brace is not the start of one.
func TestNotifyRejectsTrailingData(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	for _, body := range []string{
		`{"title":"a","message":"b"}}`,
		`{"title":"a","message":"b"}]`,
		`{"title":"a","message":"b"} {"title":"c","message":"d"}`,
		`{"title":"a","message":"b"}garbage`,
	} {
		rec, _ := post(t, s.PublicMux(), "/v1/notify", devKey, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// RFC 7235 §2.1: the auth-scheme token is case-insensitive. Rejecting
// "bearer " 401'd a valid key and wrote an auth_fail line, making a client
// casing bug indistinguishable from someone guessing keys.
func TestAuthSchemeIsCaseInsensitive(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	for _, scheme := range []string{"bearer", "BEARER", "BeArEr"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/notify", strings.NewReader(`{"title":"a","message":"b"}`))
		req.Header.Set("Authorization", scheme+" "+devKey)
		rec := httptest.NewRecorder()
		s.PublicMux().ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("scheme %q: 401 for a valid key", scheme)
		}
	}
}

func TestRateCap(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")
	s.Rate = NewRateLimiter(1, time.Hour)

	post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b"}`)
	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	// Rounded up, not to nearest: a machine that obeys Retry-After to the second
	// must not land back inside the same window and get a second 429.
	s.Rate = NewRateLimiter(1, 1400*time.Millisecond)
	post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b"}`)
	rec, _ = post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b"}`)
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q with ~1.4s of window left, want \"2\"", got)
	}
	if body["result"] != "rate_capped" {
		t.Errorf("result = %v", body["result"])
	}
}

func TestHealthAndMetrics(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/health = %d", rec.Code)
	}

	post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b"}`)
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec = httptest.NewRecorder()
	s.OpsMux().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `notify_requests_total{outcome="delivered"} 1`) {
		t.Errorf("metrics missing request counter:\n%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `notify_deliveries_total{channel="ntfy",outcome="ok"} 1`) {
		t.Errorf("metrics missing delivery counter:\n%s", rec.Body)
	}
}

func TestTestCTA(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, body := post(t, s.OpsMux(), "/test/ntfy", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test CTA = %d body = %s", rec.Code, rec.Body)
	}
	if body["result"] != "delivered" {
		t.Errorf("result = %v", body["result"])
	}

	rec, _ = post(t, s.OpsMux(), "/test/nope", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel test = %d, want 404", rec.Code)
	}
}

// A repeated channel id must be delivered once and must not pin the handler
// for the whole response window waiting on an outcome that can never arrive
// (the registry keys waiters on request+channel, so the second enqueue has
// nobody left to resolve).
func TestNotifyDeduplicatesRepeatedChannel(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	start := time.Now()
	rec, body := post(t, s.PublicMux(), "/v1/notify", devKey, `{"title":"a","message":"b","channels":["ntfy","ntfy"]}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if body["result"] != "delivered" {
		t.Errorf("result = %v", body["result"])
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream delivered %d times, want 1 — a duplicate id must not double-buzz the phone", got)
	}
	if elapsed >= s.Window {
		t.Errorf("handler took %v (window %v): a duplicate id stalled the response", elapsed, s.Window)
	}
}

// Tags go through the same ingest choke-point as title and message. The ntfy
// adapter JSON-encodes them so the wire is safe today, but the exec adapter
// hands them to a subprocess.
func TestNotifyTagsAreSanitised(t *testing.T) {
	bodies := make(chan []byte, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
	}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	rec, _ := post(t, s.PublicMux(), "/v1/notify", devKey,
		"{\"title\":\"a\",\"message\":\"b\",\"tags\":[\"de\\u001b[31mploy\"]}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}

	var payload struct {
		Tags []string `json:"tags"`
	}
	select {
	case b := <-bodies:
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the publish")
	}
	if len(payload.Tags) != 1 {
		t.Fatalf("tags = %v", payload.Tags)
	}
	if strings.ContainsRune(payload.Tags[0], 0x1b) {
		t.Errorf("tag %q still carries an ESC; tags must be stripped at ingest like titles", payload.Tags[0])
	}
	if payload.Tags[0] != "de[31mploy" {
		t.Errorf("tag = %q, want the control char stripped and the rest kept", payload.Tags[0])
	}
}

// A zero cap is rejected by config, but the limiter must not be a landmine if
// one ever reaches it: indexing an empty slice panicked every notify request
// while /health stayed green.
func TestRateLimiterZeroCapDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Allow panicked with a zero cap: %v", r)
		}
	}()
	for _, c := range []int{0, -1} {
		rl := NewRateLimiter(c, time.Hour)
		if ok, _ := rl.Allow(time.Now()); !ok {
			t.Errorf("cap %d: want uncapped fallback, got a rejection", c)
		}
	}
}
