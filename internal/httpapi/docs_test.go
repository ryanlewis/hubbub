package httpapi

import (
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The docs page renders the served spec, so the checks that matter are about
// that relationship: everything the spec documents reaches the page, nothing
// the page shows was invented locally, and the one input on it that takes a
// credential is not left casually exposed.

func docsBody(t *testing.T, s *Server, headers map[string]string) string {
	t.Helper()
	rec := fetch(t, s, "/docs", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs = %d, body %.200s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	return rec.Body.String()
}

// TestDocsRendersEveryDocumentedEndpoint is the check the whole design exists
// to make possible: the page is a view of the spec, so an endpoint documented
// but not rendered is a bug in the renderer, not a page someone forgot to edit.
func TestDocsRendersEveryDocumentedEndpoint(t *testing.T) {
	s := landingServer(t)

	spec := servedSpec(t, s, nil)
	paths, _ := spec["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("spec documents no paths")
	}

	body := docsBody(t, s, nil)
	for path, item := range paths {
		if !strings.Contains(body, ">"+path+"<") {
			t.Errorf("spec documents %s but the page does not show it", path)
		}
		methods, _ := item.(map[string]any)
		for method, raw := range methods {
			op, _ := raw.(map[string]any)
			// Escaped before comparing: the summaries carry apostrophes, and
			// html/template rightly emits them as &#39;.
			if summary, _ := op["summary"].(string); summary != "" &&
				!strings.Contains(body, template.HTMLEscapeString(summary)) {
				t.Errorf("%s %s: summary is not on the page", strings.ToUpper(method), path)
			}
			resp, _ := op["responses"].(map[string]any)
			for code := range resp {
				if !strings.Contains(body, ">"+code+"<") {
					t.Errorf("%s %s: documented status %s is not on the page", strings.ToUpper(method), path, code)
				}
			}
		}
	}
}

// TestDocsRendersRequestSchema pins the request table to the schema rather than
// to prose: a field added to NotifyRequest that nobody documents on the page is
// a field callers never find out about.
func TestDocsRendersRequestSchema(t *testing.T) {
	s := landingServer(t)
	body := docsBody(t, s, nil)

	props, ok := dig(servedSpec(t, s, nil), "components", "schemas", "NotifyRequest", "properties")
	if !ok {
		t.Fatal("spec has no NotifyRequest.properties")
	}
	for name := range props {
		if !strings.Contains(body, ">"+name+"<") {
			t.Errorf("request field %q is not in the rendered table", name)
		}
	}

	// The constraint column is the reason to render a table at all rather than
	// dumping the schema — a caller should not have to read a paragraph to find
	// the length limit.
	for _, rule := range []string{"≤ 256 bytes", "≤ 4096 bytes", "≤ 16 items", "low | default | high | urgent"} {
		if !strings.Contains(body, rule) {
			t.Errorf("constraint %q is not rendered", rule)
		}
	}
}

// TestDocsShowsTheChannelEnumFromConfig: the injected channel list lives on
// `channels.items`, not on the field, so reading only the field's own enum
// drops the one value list that is specific to this deployment.
func TestDocsShowsTheChannelEnumFromConfig(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "[alpha]\ntype = \"ntfy\"\nserver = \""+up.URL+
		"\"\ntopic = \"a\"\n[bravo]\ntype = \"ntfy\"\nenabled = false\n")

	if body := docsBody(t, s, nil); !strings.Contains(body, "alpha | bravo") {
		t.Error("the configured channel ids are not rendered on the channels field")
	}
}

// TestDocsExamplesAreSendableAsRendered. The example is not decoration: it is
// the initial contents of a textarea whose next stop is the real handler, so
// what the page prints has to be something the decoder accepts.
//
// Field *order* is not asserted, and deliberately. Both this page and
// /openapi.json build on a map[string]any, and encoding/json emits map keys
// sorted, so the served spec has always alphabetised example objects relative
// to the checked-in file — `message` prints before `title`. It is invisible to
// every JSON parser, and the alternative is threading json.RawMessage through
// five levels of the resolver to preserve something no caller can observe.
func TestDocsExamplesAreSendableAsRendered(t *testing.T) {
	s := landingServer(t)
	body := docsBody(t, s, nil)

	notify := section(t, body, "post-v1-notify")
	start := strings.Index(notify, "<textarea")
	if start < 0 {
		t.Fatal("the send panel has no request body textarea")
	}
	start = strings.Index(notify[start:], ">") + start + 1
	end := strings.Index(notify[start:], "</textarea>")
	if end < 0 {
		t.Fatal("unterminated textarea")
	}
	rendered := html.UnescapeString(notify[start : start+end])

	rec, parsed := post(t, s.PublicMux(), "/v1/notify", devKey, rendered)
	if rec.Code == http.StatusBadRequest {
		t.Errorf("the example as rendered is rejected by the handler: %v\n%s", parsed["error"], rendered)
	}
}

// TestDocsSendPanelIsGuarded. The page carries a live Send button, and on a
// notification hub that means a real push. Two things have to hold: the
// consequence is stated, and the key field never becomes something a browser
// or a shared machine keeps.
func TestDocsSendPanelIsGuarded(t *testing.T) {
	s := landingServer(t)
	body := docsBody(t, s, nil)

	if !strings.Contains(body, "delivers a real notification") {
		t.Error("the send panel does not warn that it fires a real notification")
	}
	if !strings.Contains(body, `type="password"`) || !strings.Contains(body, `autocomplete="off"`) {
		t.Error("the key field should be a password input with autocomplete off")
	}
	// Persisting it would turn a page visit into a key left on the machine.
	for _, bad := range []string{"localStorage", "sessionStorage", "document.cookie"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page uses %s — the key must not outlive the tab", bad)
		}
	}
}

// TestDocsCarriesANonceScopedCSP: this is the one page in hubbub where a key is
// typed, so it is the one worth a policy. A nonce that repeated across requests
// would be no better than 'unsafe-inline'.
func TestDocsCarriesANonceScopedCSP(t *testing.T) {
	s := landingServer(t)

	nonceOf := func() string {
		rec := fetch(t, s, "/docs", nil)
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Fatal("no Content-Security-Policy header")
		}
		for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP is missing %q: %s", want, csp)
			}
		}
		_, rest, ok := strings.Cut(csp, "script-src 'nonce-")
		if !ok {
			t.Fatalf("CSP has no script nonce: %s", csp)
		}
		nonce, _, _ := strings.Cut(rest, "'")
		if nonce == "" {
			t.Fatal("empty nonce")
		}
		// A policy naming a nonce the page doesn't carry blocks its own script.
		if !strings.Contains(rec.Body.String(), `nonce="`+nonce+`"`) {
			t.Error("the CSP nonce does not appear on the page's script tag")
		}
		return nonce
	}

	if a, b := nonceOf(), nonceOf(); a == b {
		t.Errorf("the nonce is reused across requests (%s)", a)
	}
}

func TestDocsShowsTheHostItWasReachedOn(t *testing.T) {
	s := landingServer(t)
	body := docsBody(t, s, map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "hub.example.com",
	})
	if !strings.Contains(body, "https://hub.example.com") {
		t.Error("the page does not name the base URL it was reached on")
	}
}

// TestDocsMarksAuthPerOperation: an operation-level `"security": []` opts out
// of the document's default requirement, and collapsing "absent" into "empty"
// would badge /health as needing a key — which would send a reader hunting for
// a credential to run a liveness check.
func TestDocsMarksAuthPerOperation(t *testing.T) {
	s := landingServer(t)
	body := docsBody(t, s, nil)

	notify := section(t, body, "post-v1-notify")
	if !strings.Contains(notify, "bearer key required") {
		t.Error("POST /v1/notify is not marked as needing a key")
	}
	health := section(t, body, "get-health")
	if strings.Contains(health, "bearer key required") {
		t.Error("GET /health is marked as needing a key, but the spec opts it out")
	}
	if !strings.Contains(health, "no auth") {
		t.Error("GET /health is not marked as unauthenticated")
	}
}

// section returns the rendered block for one endpoint anchor.
func section(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("no section with id %q", id)
	}
	rest := body[start:]
	if end := strings.Index(rest, `<section class="endpoint"`); end > 0 {
		return rest[:end]
	}
	return rest
}
