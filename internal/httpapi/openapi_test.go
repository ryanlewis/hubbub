package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// The tests below are the drift check the served spec exists to survive: a
// hand-written document describing a handler nobody re-reads rots into a lie,
// and a lie about the request shape is worse than no spec at all — an agent
// builds exactly what it is told and gets a 400 it cannot diagnose. So the spec
// is asserted against the real handler rather than reviewed by eye.

// servedSpec fetches /openapi.json through the real public mux.
func servedSpec(t *testing.T, s *Server, headers map[string]string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d, body %s", rec.Code, rec.Body)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served spec is not JSON: %v", err)
	}
	return doc
}

// notifyOp returns the POST /v1/notify operation object from a served spec.
func notifyOp(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	op, ok := dig(doc, "paths", "/v1/notify", "post")
	if !ok {
		t.Fatal("spec has no POST /v1/notify operation")
	}
	return op
}

func TestOpenAPISpecIsValidJSONAndStructured(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(openapiSpec, &doc); err != nil {
		t.Fatalf("embedded openapi.json does not parse: %v", err)
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.") {
		t.Errorf("openapi version = %v, want 3.x", doc["openapi"])
	}
	for _, k := range []string{"info", "paths", "components"} {
		if _, ok := doc[k].(map[string]any); !ok {
			t.Errorf("spec is missing the %q object", k)
		}
	}
}

func TestOpenAPIIsServedWithoutAKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	// No Authorization header at all: an agent has to be able to read the
	// contract before it has been issued a key.
	doc := servedSpec(t, s, nil)
	if _, ok := doc["paths"].(map[string]any); !ok {
		t.Fatal("served document has no paths")
	}
}

// TestOpenAPIVersionTracksTheBinary guards the one field most likely to be
// forgotten: a spec claiming a version the binary isn't makes a deployment
// impossible to identify from the outside.
func TestOpenAPIVersionTracksTheBinary(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	doc := servedSpec(t, s, nil)
	info, _ := doc["info"].(map[string]any)
	if got := info["version"]; got != Version {
		t.Errorf("spec info.version = %v, binary Version = %q", got, Version)
	}
}

// TestOpenAPIPathsAreRouted catches a documented endpoint that no longer
// exists — the failure an agent hits as a bare 404 with nothing to explain it.
func TestOpenAPIPathsAreRouted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")
	mux := s.PublicMux()

	doc := servedSpec(t, s, nil)
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("spec documents no paths")
	}

	for path, item := range paths {
		methods, ok := item.(map[string]any)
		if !ok {
			t.Errorf("%s: path item is not an object", path)
			continue
		}
		for method := range methods {
			req := httptest.NewRequest(strings.ToUpper(method), path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+devKey)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			// Any handler-level answer proves the route exists; only the mux's
			// own "nothing here" verdicts are drift.
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("spec documents %s %s but the mux answers %d", strings.ToUpper(method), path, rec.Code)
			}
		}
	}
}

// TestOpenAPIExamplesAreAcceptedByTheHandler is the core contract check: every
// example request in the spec is replayed against the real handler. Because the
// decoder rejects unknown fields, an example carrying a field the handler no
// longer knows fails here rather than in a caller's logs.
func TestOpenAPIExamplesAreAcceptedByTheHandler(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	doc := servedSpec(t, s, nil)
	op := notifyOp(t, doc)
	examples, ok := dig(op, "requestBody", "content", "application/json", "examples")
	if !ok || len(examples) == 0 {
		t.Fatal("POST /v1/notify documents no request examples: the drift check has nothing to verify")
	}

	documented := documentedStatuses(t, op)
	for name, ex := range examples {
		exObj, ok := ex.(map[string]any)
		if !ok {
			t.Errorf("example %q is not an object", name)
			continue
		}
		body, err := json.Marshal(exObj["value"])
		if err != nil {
			t.Errorf("example %q has an unmarshalable value: %v", name, err)
			continue
		}
		rec, parsed := post(t, s.PublicMux(), "/v1/notify", devKey, string(body))
		if rec.Code == http.StatusBadRequest {
			t.Errorf("example %q is rejected by the handler: %v (body %s)", name, parsed["error"], body)
			continue
		}
		if !documented[rec.Code] {
			t.Errorf("example %q produced status %d, which the spec does not document", name, rec.Code)
		}
	}
}

// TestOpenAPIRequestSchemaMatchesTheDecoder pins the documented field set to
// the struct the decoder actually fills. Examples alone only catch fields the
// spec has and the handler doesn't; this catches the reverse — a field added to
// the API that nobody wrote down, which is invisible to every caller.
func TestOpenAPIRequestSchemaMatchesTheDecoder(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	props, ok := dig(servedSpec(t, s, nil), "components", "schemas", "NotifyRequest", "properties")
	if !ok {
		t.Fatal("spec has no NotifyRequest.properties")
	}

	var documented []string
	for name := range props {
		documented = append(documented, name)
	}
	var actual []string
	for f := range reflect.TypeFor[notifyRequest]().Fields() {
		tag := f.Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			actual = append(actual, name)
		}
	}
	sort.Strings(documented)
	sort.Strings(actual)
	if !reflect.DeepEqual(documented, actual) {
		t.Errorf("NotifyRequest properties drifted:\n  spec:    %v\n  decoder: %v", documented, actual)
	}
}

// The caps the handler enforces are byte caps, and JSON Schema's maxLength
// counts characters — a 256-emoji title validates clean against maxLength: 256
// and is then rejected by the server as roughly a kilobyte. So the spec carries
// them as an x- extension, which validators ignore rather than assert wrongly,
// and this pins the numbers to the constants they describe.
func TestOpenAPIByteCapsMatchTheValidator(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	spec := servedSpec(t, s, nil)
	props, ok := dig(spec, "components", "schemas", "NotifyRequest", "properties")
	if !ok {
		t.Fatal("spec has no NotifyRequest.properties")
	}

	for field, want := range map[string]int{
		"title":   notify.MaxTitleLen,
		"message": notify.MaxMessageLen,
		"html":    notify.MaxHTMLLen,
	} {
		schema, _ := props[field].(map[string]any)
		got, ok := schema["x-max-bytes"].(float64)
		if !ok {
			t.Errorf("%s has no x-max-bytes; a byte cap nobody documents is a 400 nobody predicted", field)
			continue
		}
		if int(got) != want {
			t.Errorf("%s x-max-bytes = %d, want %d", field, int(got), want)
		}
		if _, wrong := schema["maxLength"]; wrong {
			t.Errorf("%s carries maxLength, which counts characters and disagrees with the server", field)
		}
	}

	tags, _ := props["tags"].(map[string]any)
	if items, _ := tags["items"].(map[string]any); items != nil {
		if got, ok := items["x-max-bytes"].(float64); !ok || int(got) != notify.MaxTagLen {
			t.Errorf("tags item x-max-bytes = %v, want %d", items["x-max-bytes"], notify.MaxTagLen)
		}
	}
	if got, ok := tags["maxItems"].(float64); !ok || int(got) != notify.MaxTags {
		t.Errorf("tags maxItems = %v, want %d", tags["maxItems"], notify.MaxTags)
	}
}

// TestOpenAPIPriorityEnumMatchesTheParser checks both directions: everything
// documented parses, and nothing outside the enum sneaks through. A spec
// listing a priority the parser rejects turns a by-the-book caller into a 400.
func TestOpenAPIPriorityEnumMatchesTheParser(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	schema, ok := dig(servedSpec(t, s, nil), "components", "schemas", "NotifyRequest", "properties", "priority")
	if !ok {
		t.Fatal("spec has no priority schema")
	}
	rawEnum, _ := schema["enum"].([]any)
	if len(rawEnum) == 0 {
		t.Fatal("priority has no enum")
	}

	documented := map[string]bool{}
	for _, v := range rawEnum {
		s, _ := v.(string)
		documented[s] = true
		p, err := notify.ParsePriority(s)
		if err != nil {
			t.Errorf("spec documents priority %q but the parser rejects it: %v", s, err)
			continue
		}
		if string(p) != s {
			t.Errorf("priority %q parses to %q — the enum should list canonical values", s, p)
		}
	}
	// The parser's own vocabulary, so a new priority added in code without a
	// spec update fails here instead of being undiscoverable.
	for _, p := range []notify.Priority{
		notify.PriorityLow, notify.PriorityDefault, notify.PriorityHigh, notify.PriorityUrgent,
	} {
		if !documented[string(p)] {
			t.Errorf("parser accepts priority %q but the spec does not document it", p)
		}
	}
	if _, err := notify.ParsePriority("critical"); err == nil {
		t.Error("parser accepted a priority outside the documented enum")
	}
}

// TestOpenAPIDocumentsEveryStatusTheHandlerReturns drives the real scenarios
// behind each documented status code. The response contract is the part a
// machine caller branches on, so an undocumented status is a caller that
// cannot tell "retry later" from "you sent nonsense".
func TestOpenAPIDocumentsEveryStatusTheHandlerReturns(t *testing.T) {
	cases := []struct {
		name     string
		upstream int // status the fake ntfy returns
		key      string
		body     string
		rateCap  int // >0 installs a limiter this small
		primeCap bool
		want     int
	}{
		{name: "delivered", upstream: 200, key: devKey, body: `{"title":"t","message":"m"}`, want: http.StatusOK},
		{name: "queued", upstream: 500, key: devKey, body: `{"title":"t","message":"m"}`, want: http.StatusAccepted},
		{name: "failed", upstream: 400, key: devKey, body: `{"title":"t","message":"m"}`, want: http.StatusBadGateway},
		{name: "malformed", upstream: 200, key: devKey, body: `{"title":"t","message":"m","nope":1}`, want: http.StatusBadRequest},
		{name: "unauthorized", upstream: 200, key: "", body: `{"title":"t","message":"m"}`, want: http.StatusUnauthorized},
		{name: "forbidden", upstream: 200, key: devKey, body: `{"title":"t","message":"m","channels":["other"]}`, want: http.StatusForbidden},
		{name: "rateCapped", upstream: 200, key: devKey, body: `{"title":"t","message":"m"}`, rateCap: 1, primeCap: true, want: http.StatusTooManyRequests},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.upstream)
			}))
			defer up.Close()
			s := newTestServer(t, up.URL, "")
			if tc.rateCap > 0 {
				s.Rate = NewRateLimiter(tc.rateCap, time.Hour)
			}
			mux := s.PublicMux()
			if tc.primeCap {
				post(t, mux, "/v1/notify", tc.key, `{"title":"t","message":"m"}`)
			}

			rec, _ := post(t, mux, "/v1/notify", tc.key, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if !documentedStatuses(t, notifyOp(t, servedSpec(t, s, nil)))[rec.Code] {
				t.Errorf("handler returns %d but the spec does not document it", rec.Code)
			}
		})
	}
}

func documentedStatuses(t *testing.T, op map[string]any) map[int]bool {
	t.Helper()
	resp, ok := op["responses"].(map[string]any)
	if !ok {
		t.Fatal("operation documents no responses")
	}
	out := map[int]bool{}
	for code := range resp {
		n, err := strconv.Atoi(code)
		if err != nil {
			t.Errorf("response key %q is not a status code", code)
			continue
		}
		out[n] = true
	}
	return out
}

// TestOpenAPIChannelEnumFollowsConfig covers the reason the enum is injected at
// all: channel ids are per-deployment, so a hand-written list would be wrong
// everywhere but the author's own hub.
func TestOpenAPIChannelEnumFollowsConfig(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()

	channels := "[ntfy]\ntype = \"ntfy\"\nserver = \"" + up.URL + "\"\ntopic = \"tst\"\n" +
		"[parked]\ntype = \"ntfy\"\nenabled = false\n" +
		"[alpha]\ntype = \"ntfy\"\nserver = \"" + up.URL + "\"\ntopic = \"a\"\n"
	s := newTestServer(t, up.URL, channels)

	items, ok := dig(servedSpec(t, s, nil), "components", "schemas", "NotifyRequest", "properties", "channels", "items")
	if !ok {
		t.Fatal("spec has no channels.items schema")
	}
	var got []string
	for _, v := range items["enum"].([]any) {
		got = append(got, v.(string))
	}
	// Sorted, and the disabled channel is present: naming it is not an error
	// but a per-channel "disabled" result, so it is a legitimate value to send.
	want := []string{"alpha", "ntfy", "parked"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("channel enum = %v, want %v", got, want)
	}
}

// TestOpenAPIChannelEnumOmittedWhenNoChannels: an empty enum is a schema no
// value satisfies, which reads as "this field is unusable" rather than "this
// hub has no channels configured yet".
func TestOpenAPIChannelEnumOmittedWhenNoChannels(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "# no channels configured\n")

	items, ok := dig(servedSpec(t, s, nil), "components", "schemas", "NotifyRequest", "properties", "channels", "items")
	if !ok {
		t.Fatal("spec has no channels.items schema")
	}
	if _, present := items["enum"]; present {
		t.Errorf("enum should be absent with no channels, got %v", items["enum"])
	}
}

func TestOpenAPIServerURLReflectsHowItWasReached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := newTestServer(t, up.URL, "")

	serverURL := func(headers map[string]string) string {
		doc := servedSpec(t, s, headers)
		servers, _ := doc["servers"].([]any)
		if len(servers) != 1 {
			t.Fatalf("servers = %v, want exactly one", servers)
		}
		entry, _ := servers[0].(map[string]any)
		url, _ := entry["url"].(string)
		return url
	}

	// Behind a TLS-terminating proxy the request itself is plain http; taking
	// the scheme from the connection would advertise an http:// base for an
	// https-only deployment.
	if got := serverURL(map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "hub.example.com",
	}); got != "https://hub.example.com" {
		t.Errorf("proxied server url = %q", got)
	}

	// Proxies append rather than replace, so the chain can be several deep.
	if got := serverURL(map[string]string{
		"X-Forwarded-Proto": "https, http",
		"X-Forwarded-Host":  "hub.example.com, internal:8080",
	}); got != "https://hub.example.com" {
		t.Errorf("chained forwarded headers = %q", got)
	}

	// Unproxied, the Host header stands on its own.
	if got := serverURL(nil); got != "http://example.com" {
		t.Errorf("direct server url = %q", got)
	}

	// The headers are caller-controlled when nothing sits in front of the hub;
	// junk must not reach the served document as URL structure.
	for _, bad := range []string{"evil.test/path", "host with spaces", "user@evil.test"} {
		got := serverURL(map[string]string{"X-Forwarded-Host": bad})
		if strings.Contains(got, bad) {
			t.Errorf("forwarded host %q leaked into server url %q", bad, got)
		}
	}
}
