package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The delivery log is every caller's notification content, so these tests are
// mostly about who is refused. The rest asserts the shape a polling dashboard
// depends on: an array that is always there, newest first, fields always
// present.

const readerKey = "nh_test_reader_0123456789"

// recentKeys is one caller that may only send and one that may only read —
// the two halves of the permission this endpoint introduces.
const recentKeys = `
[dev]
key = "` + devKey + `"
channels = ["ntfy"]

[dash]
key = "` + readerKey + `"
channels = []
recent = true
`

// recentServer returns the hub and the directory holding its delivery log, so
// a test can assert on the lines a refusal leaves behind.
func recentServer(t *testing.T, upstreamURL string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return newTestServerWithKeys(t, dir, upstreamURL, "", recentKeys), dir
}

func getRecent(t *testing.T, s *Server, path, key string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestRecentRequiresACredential(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)

	rec, _ := getRecent(t, s, "/v1/recent", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body)
	}
	rec, _ = getRecent(t, s, "/v1/recent", "nh_not_a_real_key_at_all")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d, want 401", rec.Code)
	}
}

// The whole reason the grant exists: a key that can send is not thereby a key
// that can read what everyone else sent.
func TestRecentRefusesASendOnlyKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)

	rec, body := getRecent(t, s, "/v1/recent", devKey)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body)
	}
	// The refusal has to name the fix — the operator reading this is the one
	// who would go and add the line.
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("403 carried no error message")
	}
}

// A refused read is what a leaked credential looks like from inside the hub.
func TestRecentRefusalsAreLogged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, dir := recentServer(t, up.URL)

	getRecent(t, s, "/v1/recent", devKey)
	getRecent(t, s, "/v1/recent", "")

	var fails []map[string]any
	for _, l := range readLog(t, filepath.Join(dir, "keys.toml")) {
		if l["kind"] == "auth_fail" {
			fails = append(fails, l)
		}
	}
	if len(fails) != 2 {
		t.Fatalf("got %d auth_fail lines, want one per refusal: %v", len(fails), fails)
	}
	// The refused key is named, since "which credential was this" is the whole
	// value of the line months later. The key itself never is.
	if fails[0]["callerId"] != "dev" {
		t.Errorf("refused caller = %v, want the key's caller id", fails[0]["callerId"])
	}
	for _, l := range fails {
		if detail, _ := l["detail"].(string); detail == "" {
			t.Error("refusal recorded no reason")
		}
	}
}

func TestRecentReturnsDeliveries(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)
	mux := s.PublicMux()

	if rec, _ := post(t, mux, "/v1/notify", devKey, `{"title":"Backup failed","message":"m","priority":"high"}`); rec.Code != http.StatusOK {
		t.Fatalf("seeding send = %d (%s)", rec.Code, rec.Body)
	}

	rec, body := getRecent(t, s, "/v1/recent", readerKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want the one delivery", body["items"])
	}
	if got, want := body["count"], float64(1); got != want {
		t.Errorf("count = %v, want %v", got, want)
	}

	item, _ := items[0].(map[string]any)
	for field, want := range map[string]string{
		"caller":   "dev",
		"channel":  "ntfy",
		"title":    "Backup failed",
		"priority": "high",
		"outcome":  "ok",
	} {
		if got, _ := item[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	// Every field is present even when empty, so a consumer can index the shape
	// unconditionally.
	for _, field := range []string{"ts", "settledAt", "requestId", "caller", "channel", "title", "priority", "outcome"} {
		if _, ok := item[field]; !ok {
			t.Errorf("item is missing %q: %v", field, item)
		}
	}
	if ts, _ := item["ts"].(string); ts == "" {
		t.Error("ts is empty")
	}
}

// An empty log answers with an empty array, never null and never a 404 — a
// widget that has to special-case "no news yet" is a widget that breaks on its
// first quiet day.
func TestRecentOnAQuietHubIsAnEmptyArray(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)

	rec, _ := getRecent(t, s, "/v1/recent", readerKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Items *[]recentItem `json:"items"`
		Count int           `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Items == nil {
		t.Fatalf("items was null, not []: %s", rec.Body)
	}
	if len(*body.Items) != 0 || body.Count != 0 {
		t.Errorf("body = %s, want an empty page", rec.Body)
	}
}

func TestRecentQueryValidation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"defaults", "/v1/recent", http.StatusOK},
		{"limit", "/v1/recent?limit=10", http.StatusOK},
		{"since", "/v1/recent?since=2026-08-08T09:00:00Z", http.StatusOK},
		{"channel", "/v1/recent?channel=ntfy", http.StatusOK},
		{"all three", "/v1/recent?limit=1&since=2020-01-01T00:00:00Z&channel=ntfy", http.StatusOK},
		{"limit not a number", "/v1/recent?limit=lots", http.StatusBadRequest},
		{"limit zero", "/v1/recent?limit=0", http.StatusBadRequest},
		// Clamping instead would answer a request for 5000 with 500 and no way
		// to tell that from the log holding only that many.
		{"limit past the cap", "/v1/recent?limit=5000", http.StatusBadRequest},
		{"since not a timestamp", "/v1/recent?since=yesterday", http.StatusBadRequest},
		// A misspelled filter that silently returns everything is a filter the
		// caller believes is applied.
		{"unknown parameter", "/v1/recent?chanel=ntfy", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := getRecent(t, s, tc.query, readerKey)
			if rec.Code != tc.want {
				t.Errorf("%s = %d, want %d (body %s)", tc.query, rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestRecentLimitAndFiltersReachTheLog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)
	mux := s.PublicMux()

	for _, title := range []string{"first", "second", "third"} {
		post(t, mux, "/v1/notify", devKey, `{"title":"`+title+`","message":"m"}`)
	}

	_, body := getRecent(t, s, "/v1/recent?limit=2", readerKey)
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("limit=2 returned %d items", len(items))
	}
	first, _ := items[0].(map[string]any)
	if got, _ := first["title"].(string); got != "third" {
		t.Errorf("newest item = %q, want the last one sent", got)
	}

	_, body = getRecent(t, s, "/v1/recent?channel=nope", readerKey)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("filtering on an unused channel returned %v", items)
	}
}

// The operator's own browser reads the log with the identity that already
// administers the hub, rather than needing a key minted for a person.
func TestRecentAcceptsAnAllowlistedAdminIdentity(t *testing.T) {
	s, _, _ := adminServer(t)

	rec := adminGet(t, s, "/v1/recent", adminEmail)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	// Authenticated but not on the allowlist is refused like anyone else: the
	// dashboard's allowlist is the boundary, not merely having a proxy session.
	if rec := adminGet(t, s, "/v1/recent", "stranger@example.com"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unpermitted identity = %d, want 401", rec.Code)
	}
	if rec := adminGet(t, s, "/v1/recent", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", rec.Code)
	}
}

// A presented key is answered on its own merits. Falling back to the browser
// identity would tell a machine its key worked when it did not, and the next
// poll from anywhere else would fail with nothing to explain it.
func TestRecentPrefersThePresentedKeyOverTheIdentity(t *testing.T) {
	s, _, _ := adminServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/recent", nil)
	req.Header.Set("Authorization", "Bearer "+devKey) // valid, but send-only
	req.Header.Set("X-ExeDev-Email", adminEmail)
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want the key's own 403 (body %s)", rec.Code, rec.Body)
	}
}

// The body is every caller's titles and caller ids — the same content the
// dashboard sets no-store for, and on the admin-identity path there is no
// Authorization header to make a cache treat it as private on its own.
func TestRecentIsNeverCached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)

	for _, key := range []string{readerKey, devKey, ""} {
		rec, _ := getRecent(t, s, "/v1/recent", key)
		if got := rec.Header().Get("Cache-Control"); got != "no-store, private" {
			t.Errorf("key %q: Cache-Control = %q", key, got)
		}
	}
}

// A valid key used outside its grant is a misconfigured widget, not someone
// guessing keys — counting it as an auth failure pages the operator for the
// wrong thing. Reads get their own series instead.
func TestRecentRefusalsAreCountedApart(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)

	getRecent(t, s, "/v1/recent", devKey) // 403: permission, not credential
	if got := s.Metrics.Render(); strings.Contains(got, "notify_auth_failures_total 0") == false {
		t.Errorf("a permission refusal was counted as an auth failure:\n%s", got)
	}

	getRecent(t, s, "/v1/recent", "nh_wrong_key_0123456789") // 401: credential
	getRecent(t, s, "/v1/recent", readerKey)                 // 200
	got := s.Metrics.Render()
	for _, want := range []string{
		"notify_auth_failures_total 1",
		`notify_recent_reads_total{outcome="forbidden"} 1`,
		`notify_recent_reads_total{outcome="ok"} 1`,
		`notify_recent_reads_total{outcome="unauthorized"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Reading every caller's notification content is the same class of privilege as
// changing a grant, which the dashboard already records. Leaving only refusals
// in the log would make the reads that actually disclosed something the
// invisible ones.
func TestRecentReadsAreAudited(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, dir := recentServer(t, up.URL)

	getRecent(t, s, "/v1/recent?limit=10&channel=ntfy", readerKey)

	var reads []map[string]any
	for _, l := range readLog(t, filepath.Join(dir, "keys.toml")) {
		if l["kind"] == "read" {
			reads = append(reads, l)
		}
	}
	if len(reads) != 1 {
		t.Fatalf("got %d read lines, want one: %v", len(reads), reads)
	}
	if reads[0]["callerId"] != "dash" {
		t.Errorf("read attributed to %v, want the key's caller", reads[0]["callerId"])
	}
	// The query, never the rows it returned.
	detail, _ := reads[0]["detail"].(string)
	for _, want := range []string{"limit=10", `channel="ntfy"`, "returned="} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, missing %q", detail, want)
		}
	}
}

// A cursor a caller round-trips through this API has to exclude the row it
// names, or the newest delivery comes back on every poll for ever.
func TestRecentTimestampsWorkAsACursor(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)
	mux := s.PublicMux()

	post(t, mux, "/v1/notify", devKey, `{"title":"one","message":"m"}`)
	_, body := getRecent(t, s, "/v1/recent", readerKey)
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("seeding read = %v", body)
	}
	first, _ := items[0].(map[string]any)
	cursor, _ := first["ts"].(string)

	_, body = getRecent(t, s, "/v1/recent?since="+url.QueryEscape(cursor), readerKey)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("polling with the newest ts returned it again: %v", items)
	}

	post(t, mux, "/v1/notify", devKey, `{"title":"two","message":"m"}`)
	_, body = getRecent(t, s, "/v1/recent?since="+url.QueryEscape(cursor), readerKey)
	items, _ = body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("the new delivery did not show up: %v", body)
	}
	if got, _ := items[0].(map[string]any)["title"].(string); got != "two" {
		t.Errorf("title = %q, want the one sent after the cursor", got)
	}
}

// The rows a limit drops are the oldest of the match, and a cursor-following
// caller's next poll moves past them — so the answer has to admit it was cut.
func TestRecentReportsTruncation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)
	mux := s.PublicMux()

	for _, title := range []string{"first", "second", "third"} {
		post(t, mux, "/v1/notify", devKey, `{"title":"`+title+`","message":"m"}`)
	}

	_, body := getRecent(t, s, "/v1/recent?limit=2", readerKey)
	if body["truncated"] != true {
		t.Errorf("limit=2 over three deliveries reported truncated = %v", body["truncated"])
	}
	_, body = getRecent(t, s, "/v1/recent?limit=50", readerKey)
	if body["truncated"] != false {
		t.Errorf("a complete answer reported truncated = %v", body["truncated"])
	}
}

// The read is not a send: it must not spend the cap that exists to bound how
// much a runaway caller can deliver.
func TestRecentDoesNotSpendTheRateCap(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := recentServer(t, up.URL)
	s.Rate = NewRateLimiter(1, time.Hour)
	mux := s.PublicMux()

	for range 5 {
		if rec, _ := getRecent(t, s, "/v1/recent", readerKey); rec.Code != http.StatusOK {
			t.Fatalf("poll was rate-capped: %d", rec.Code)
		}
	}
	if rec, _ := post(t, mux, "/v1/notify", devKey, `{"title":"t","message":"m"}`); rec.Code != http.StatusOK {
		t.Fatalf("the one allowed send = %d, want the cap untouched by the polls", rec.Code)
	}
}
