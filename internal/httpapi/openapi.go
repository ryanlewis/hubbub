package httpapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed openapi.json
var openapiSpec []byte

// handleOpenAPI serves the hand-written spec with the per-deployment parts
// filled in.
//
// Three things cannot be written down ahead of time: the channel ids are
// whatever this operator configured, the base URL is wherever this instance is
// reached, and the version belongs to the binary. Hand-maintaining any of them
// in the checked-in file guarantees it is wrong on every deployment but the
// author's — the whole point of serving a spec is that an agent pointed at the
// base URL gets the truth about *this* hub.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	// Unmarshalling per request is what makes the injection below safe: each
	// call gets its own tree, so mutating it cannot race a concurrent request
	// or leave one caller's reconstructed base URL cached for the next.
	var doc map[string]any
	if err := json.Unmarshal(openapiSpec, &doc); err != nil {
		// Embedded at build time and parsed by TestOpenAPISpecIsValidJSON, so
		// this needs a corrupt binary to reach.
		writeError(w, http.StatusInternalServerError, "spec unavailable")
		return
	}

	if info, ok := doc["info"].(map[string]any); ok {
		info["version"] = Version
	}
	doc["servers"] = []any{map[string]any{"url": baseURL(r)}}
	injectChannelEnum(doc, s.Store.Channels().IDs())

	writeJSON(w, http.StatusOK, doc)
}

// injectChannelEnum lists the channel ids this deployment actually has on the
// `channels` items schema. Disabled channels are included: naming one is not an
// error but a per-channel "disabled" result, so it is a legitimate value to
// send and belongs in the documented set.
//
// With no channels configured the enum is dropped rather than emitted empty —
// an empty enum is a schema no value can satisfy, which a generator would read
// as "this field is unusable" instead of "this hub has no channels yet".
func injectChannelEnum(doc map[string]any, ids []string) {
	items, ok := dig(doc, "components", "schemas", "NotifyRequest", "properties", "channels", "items")
	if !ok {
		return
	}
	if len(ids) == 0 {
		delete(items, "enum")
		return
	}
	vals := make([]any, len(ids))
	for i, id := range ids {
		vals[i] = id
	}
	items["enum"] = vals
}

// dig walks a chain of object keys, returning the map at the end.
func dig(m map[string]any, path ...string) (map[string]any, bool) {
	cur := m
	for _, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// baseURL reconstructs the URL the caller actually used, so the served spec
// names a base an agent can send to unmodified.
//
// The forwarded headers are load-bearing here rather than optional polish:
// behind a TLS-terminating proxy the request arrives over plain http, so a
// scheme taken from r.TLS alone would advertise an http:// base for an
// https-only deployment. They are also caller-controlled when nothing is in
// front of the hub, which is why the scheme is matched against a fixed pair
// and the host is bounded and screened — the value only ever reaches the
// requester's own copy of the document, but it should not be a place to park
// arbitrary bytes.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	switch strings.ToLower(strings.TrimSpace(firstForwarded(r.Header.Get("X-Forwarded-Proto")))) {
	case "https":
		scheme = "https"
	case "http":
		scheme = "http"
	}

	host := r.Host
	if h := firstForwarded(r.Header.Get("X-Forwarded-Host")); safeHost(h) {
		host = h
	}
	if !safeHost(host) {
		// Nothing usable to build an absolute URL from: a relative server URL
		// still resolves against whatever the agent fetched the spec from.
		return "/"
	}
	return scheme + "://" + host
}

// firstForwarded takes the leftmost value of a possibly comma-joined
// X-Forwarded-* header — proxies append, so the chain can be several deep.
func firstForwarded(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// safeHost accepts only what can appear in a host[:port] authority, which keeps
// whitespace, control characters and URL structure (a "/" or "@" that would
// re-point the base) out of the served document.
func safeHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == ':' || c == '[' || c == ']':
		default:
			return false
		}
	}
	return true
}
