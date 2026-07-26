package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fetch GETs a path through the real public mux, with no credential unless one
// is asked for.
func fetch(t *testing.T, s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)
	return rec
}

// landingServer is a hub whose configuration is distinctive enough to be
// recognised if any of it leaks onto a public page.
func landingServer(t *testing.T) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	return newTestServer(t, up.URL, "[ntfy]\ntype = \"ntfy\"\nserver = \""+up.URL+
		"\"\ntopic = \"tst\"\n[operators-pager-do-not-leak]\ntype = \"ntfy\"\nenabled = false\n")
}

// TestLandingTemplatesParse is what lets the handlers parse lazily instead of
// panicking in an init(): a template that would only fail at request time fails
// here, in the build, so the degraded path can never be reached by a binary
// that shipped.
func TestLandingTemplatesParse(t *testing.T) {
	set, err := htmlPages()
	if err != nil {
		t.Fatalf("html pages do not parse: %v", err)
	}
	// Named explicitly: ParseFS is happy to return a set that is missing the
	// page a handler asks for, and that failure would only show up as a 500 in
	// production.
	for _, name := range []string{"index.html", "docs.html", "admin.html", "theme", "nav", "navstyle"} {
		if set.Lookup(name) == nil {
			t.Errorf("template set has no %q", name)
		}
	}
	if _, err := llmsTmpl(); err != nil {
		t.Errorf("llms.txt does not parse: %v", err)
	}
}

// TestFaviconIsServedAndLinked. A favicon only works if three things agree:
// the asset is served, both pages point at it, and the CSP on /docs permits an
// image at all — and a CSP that forbids it fails silently, as a blank tab with
// the explanation buried in a console nobody has open.
func TestFaviconIsServedAndLinked(t *testing.T) {
	s := landingServer(t)

	rec := fetch(t, s, "/favicon.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon.svg = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("content-type = %q, want image/svg+xml", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<svg") {
		t.Errorf("body is not an SVG: %.60q", body)
	}

	for _, path := range []string{"/", "/docs"} {
		if !strings.Contains(fetch(t, s, path, nil).Body.String(), `href="/favicon.svg"`) {
			t.Errorf("GET %s does not link the favicon", path)
		}
	}

	if csp := fetch(t, s, "/docs", nil).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self'") {
		t.Errorf("the docs CSP would block its own favicon: %s", csp)
	}
}

func TestLandingPageIsServedAtRootAndIndexHTML(t *testing.T) {
	s := landingServer(t)

	for _, path := range []string{"/", "/index.html"} {
		rec := fetch(t, s, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s content-type = %q, want text/html", path, ct)
		}
		body := rec.Body.String()
		for _, want := range []string{"hubbub", "/v1/notify", "Authorization: Bearer", Version} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: page does not mention %q", path, want)
			}
		}
	}
}

// TestLandingPageIsNotACatchAll pins the "/{$}" registration. A bare "/" is a
// subtree pattern, and the failure it causes is nasty precisely because it
// looks like success: a caller that misspells /v1/notify gets 200 OK and a
// page of HTML instead of the 404 that would tell it what it did wrong.
func TestLandingPageIsNotACatchAll(t *testing.T) {
	s := landingServer(t)

	for _, path := range []string{"/v1/notifyy", "/nope", "/v1/", "/admin"} {
		if rec := fetch(t, s, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (body %.80s)", path, rec.Code, rec.Body)
		}
	}
	// The real endpoint on the wrong method must still be the mux's verdict,
	// not the landing page.
	if rec := fetch(t, s, "/v1/notify", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/notify = %d, want 405", rec.Code)
	}
}

// TestLandingIsServedWithoutAKey: both documents exist for readers who have no
// credential yet — a person who opened the base URL, and an agent handed the
// host and nothing else.
func TestLandingIsServedWithoutAKey(t *testing.T) {
	s := landingServer(t)

	for _, path := range []string{"/", "/llms.txt"} {
		if rec := fetch(t, s, path, nil); rec.Code != http.StatusOK {
			t.Errorf("unauthenticated GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// TestLandingRevealsNoDeploymentDetail is the property the pages are written
// to hold: they document the API, never the hub behind it. Anyone on the
// internet can read them, so a channel id, a caller id, a key or the ops
// surface appearing here is a leak, not a cosmetic bug.
func TestLandingRevealsNoDeploymentDetail(t *testing.T) {
	s := landingServer(t)

	for _, path := range []string{"/", "/llms.txt"} {
		body := fetch(t, s, path, nil).Body.String()
		for _, secret := range []string{
			devKey,                        // a caller's key
			"operators-pager-do-not-leak", // a channel id
			"/metrics",                    // the ops surface, which carries no auth of its own
			"/test/",
			"2112", // ...and the port it conventionally sits on
		} {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s leaks %q", path, secret)
			}
		}
	}
}

// TestLandingShowsTheHostItWasReachedOn: the curl example is there to be
// copied, so it has to name the host the reader actually used — behind a
// TLS-terminating proxy the request itself arrives as plain http.
func TestLandingShowsTheHostItWasReachedOn(t *testing.T) {
	s := landingServer(t)

	for _, path := range []string{"/", "/llms.txt"} {
		body := fetch(t, s, path, map[string]string{
			"X-Forwarded-Proto": "https",
			"X-Forwarded-Host":  "notify.exe.xyz",
		}).Body.String()
		if !strings.Contains(body, "https://notify.exe.xyz/v1/notify") {
			t.Errorf("GET %s does not show the proxied base URL", path)
		}

		// Those headers are caller-controlled when nothing sits in front of the
		// hub, and this page is HTML — junk must not arrive as markup.
		for _, bad := range []string{"evil.test/path", "host with spaces", `x"><script>`} {
			body := fetch(t, s, path, map[string]string{"X-Forwarded-Host": bad}).Body.String()
			if strings.Contains(body, bad) {
				t.Errorf("GET %s: forwarded host %q reached the page", path, bad)
			}
		}
	}
}

func TestLLMsTxtFollowsTheConvention(t *testing.T) {
	s := landingServer(t)

	rec := fetch(t, s, "/llms.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /llms.txt = %d, want 200", rec.Code)
	}
	// Markdown, but served as text/plain so it renders in a browser tab rather
	// than prompting a download.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}

	body := rec.Body.String()
	// llmstxt.org's shape: an H1 name, then a blockquote summary.
	if !strings.HasPrefix(body, "# hubbub\n") {
		t.Errorf("does not open with an H1 name, got %.40q", body)
	}
	if !strings.Contains(body, "\n> ") {
		t.Error("has no blockquote summary")
	}
	// The point of the file is to hand an agent somewhere more detailed to go.
	if !strings.Contains(body, "/openapi.json") {
		t.Error("does not link the machine-readable spec")
	}
}

// TestLandingRepeatsEveryDocumentedStatus is a drift check. The response
// contract is now written down in three places — the handler, the spec and
// these two pages — and the copy nobody re-reads is the one that rots. The spec
// is already pinned to the handler by openapi_test.go, so pinning the pages to
// the spec chains all three together: add a status code and the pages fail
// until they mention it.
func TestLandingRepeatsEveryDocumentedStatus(t *testing.T) {
	s := landingServer(t)

	statuses := documentedStatuses(t, notifyOp(t, servedSpec(t, s, nil)))
	if len(statuses) == 0 {
		t.Fatal("the spec documents no statuses: this check has nothing to verify")
	}

	for _, path := range []string{"/", "/llms.txt"} {
		body := fetch(t, s, path, nil).Body.String()
		for code := range statuses {
			if !strings.Contains(body, strconv.Itoa(code)) {
				t.Errorf("GET %s does not mention documented status %d", path, code)
			}
		}
	}
}
