package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// The docs page is rendered from the same document /openapi.json serves, never
// from a second description of the API kept alongside it. That is the whole
// design: a hand-written reference page is a third copy of the contract, and
// the copy nobody re-reads is the one that rots. Here the page physically
// cannot document an endpoint the spec doesn't have, or omit one it does.
//
// It is deliberately not Swagger UI or Redoc. Those are 0.8–1.6 MB of vendored,
// unreviewable minified JavaScript for a five-endpoint API, against a design
// whose one sanctioned browser asset is ~14 kB of HTMX. Rendering server-side
// with html/template — already linked in for the landing page — costs nothing
// extra in the binary and keeps the module graph at two entries.

// docsView is the whole page. Everything on it comes from the resolved spec
// except Nonce, which is per-request.
type docsView struct {
	Title       string
	Version     string
	BaseURL     string
	Description []template.HTML
	Endpoints   []endpointView
	Nonce       string
	Chrome      chromeView
}

type endpointView struct {
	ID             string
	Method         string
	MethodClass    string
	Path           string
	Summary        string
	Description    template.HTML
	Secured        bool
	RequestFields  []fieldView
	ResponseFields []fieldView
	Examples       []exampleView
	Body           string
	Responses      []responseView
	CanTry         bool
	Warn           string
}

type fieldView struct {
	Name        string
	Type        string
	Rules       string
	Required    bool
	Description template.HTML
}

type exampleView struct {
	Name    string
	Summary string
	Body    string
}

type responseView struct {
	Code        string
	Class       string
	Description template.HTML
	Example     string
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	doc, err := s.resolvedSpec(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "spec unavailable")
		return
	}
	// Round-tripped through JSON rather than read from the embedded bytes, so
	// the page is built from what a caller fetching /openapi.json would get —
	// injected channel enum, server URL and version included. Anything the spec
	// handler starts or stops doing reaches this page by the same route.
	raw, err := json.Marshal(doc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "spec unavailable")
		return
	}
	var spec oaSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		writeError(w, http.StatusInternalServerError, "spec unavailable")
		return
	}

	tmpl, err := htmlPages()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "docs unavailable")
		return
	}
	view := buildDocs(&spec)
	view.Chrome = chromeView{
		Nav:     navView{Current: "/docs", Admin: s.adminLink(r)},
		Version: view.Version,
	}
	// A CSP nonce, on this page alone: it is the only surface in hubbub where a
	// caller's key is typed in, so it is the one worth spending a header on.
	// Fresh per request — a fixed nonce is the same as no nonce.
	view.Nonce = rand.Text()
	// img-src is what the favicon needs: under default-src 'none' the tab icon
	// is an image fetch like any other, and the browser drops it silently —
	// a blank tab with the answer only in the console.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; "+
		"script-src 'nonce-"+view.Nonce+"'; style-src 'nonce-"+view.Nonce+"'; "+
		"img-src 'self'; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")

	renderPageTemplate(w, tmpl, "docs.html", view, "text/html; charset=utf-8")
}

// buildDocs flattens the spec into the shape the template renders.
func buildDocs(spec *oaSpec) docsView {
	v := docsView{
		Title:   spec.Info.Title,
		Version: spec.Info.Version,
	}
	if len(spec.Servers) > 0 {
		v.BaseURL = spec.Servers[0].URL
	}
	for _, para := range strings.Split(spec.Info.Description, "\n\n") {
		if para = strings.TrimSpace(para); para != "" {
			v.Description = append(v.Description, inlineMarkdown(para))
		}
	}

	for path, item := range spec.Paths {
		for method, op := range item {
			v.Endpoints = append(v.Endpoints, buildEndpoint(spec, path, method, op))
		}
	}
	sort.Slice(v.Endpoints, func(i, j int) bool {
		a, b := v.Endpoints[i], v.Endpoints[j]
		if ra, rb := methodRank(a.Method), methodRank(b.Method); ra != rb {
			return ra < rb
		}
		// "/" is a prefix of everything, so plain sorting floats the landing
		// page to the top of the list. It belongs at the bottom.
		if (a.Path == "/") != (b.Path == "/") {
			return b.Path == "/"
		}
		return a.Path < b.Path
	})
	return v
}

func buildEndpoint(spec *oaSpec, path, method string, op oaOperation) endpointView {
	method = strings.ToUpper(method)
	e := endpointView{
		ID:          anchor(method, path),
		Method:      method,
		MethodClass: strings.ToLower(method),
		Path:        path,
		Summary:     op.Summary,
		Description: inlineMarkdown(op.Description),
		// An operation-level `"security": []` opts out of the document's
		// default requirement. Absent means it inherits, which here means a key.
		Secured: op.Security == nil || len(*op.Security) > 0,
	}

	if op.RequestBody != nil {
		if media, ok := op.RequestBody.Content["application/json"]; ok {
			e.RequestFields = fieldsFor(media.Schema, spec)
			for name, ex := range media.Examples {
				e.Examples = append(e.Examples, exampleView{
					Name:    name,
					Summary: ex.Summary,
					Body:    prettyJSON(ex.Value),
				})
			}
			sort.Slice(e.Examples, func(i, j int) bool { return e.Examples[i].Name < e.Examples[j].Name })
			if len(e.Examples) > 0 {
				e.Body = e.Examples[0].Body
			}
		}
	}

	for code, resp := range op.Responses {
		r := responseView{
			Code:        code,
			Class:       code[:1],
			Description: inlineMarkdown(resp.Description),
		}
		if media, ok := resp.Content["application/json"]; ok {
			r.Example = prettyJSON(media.Example)
			// The response schema is worth a table once, from the first success
			// code — every status on an operation shares a handful of shapes,
			// and repeating the table under each one is noise.
			if e.ResponseFields == nil && strings.HasPrefix(code, "2") {
				e.ResponseFields = fieldsFor(media.Schema, spec)
			}
		}
		e.Responses = append(e.Responses, r)
	}
	sort.Slice(e.Responses, func(i, j int) bool { return e.Responses[i].Code < e.Responses[j].Code })

	e.CanTry = true
	if method == "POST" && path == "/v1/notify" {
		// The one button on this page with a consequence: a real push to the
		// operator's phone, a line in the delivery log, and one of the hub's
		// hourly requests spent.
		e.Warn = "Sending delivers a real notification to this hub's channels, writes a line to the delivery log, and spends one of the hourly rate cap."
	}
	return e
}

// fieldsFor turns an object schema into table rows.
//
// Required fields come first, in the order the schema's `required` array lists
// them, then everything else alphabetically. Unmarshalling into a map loses the
// document's property order, and alphabetical alone would open the request
// table with `channels` and bury `title` — the `required` array happens to
// carry the order a human wrote the schema in, so it is the better key.
func fieldsFor(s *oaSchema, spec *oaSpec) []fieldView {
	s = deref(s, spec)
	if s == nil || len(s.Properties) == 0 {
		return nil
	}

	required := map[string]bool{}
	order := make([]string, 0, len(s.Properties))
	for _, name := range s.Required {
		if _, ok := s.Properties[name]; ok {
			required[name] = true
			order = append(order, name)
		}
	}
	rest := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		if !required[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)

	fields := make([]fieldView, 0, len(s.Properties))
	for _, name := range append(order, rest...) {
		p := deref(s.Properties[name], spec)
		fields = append(fields, fieldView{
			Name:        name,
			Type:        typeLabel(p),
			Rules:       rules(p),
			Required:    required[name],
			Description: inlineMarkdown(p.Description),
		})
	}
	return fields
}

func typeLabel(s *oaSchema) string {
	if s == nil {
		return ""
	}
	if s.Type == "array" && s.Items != nil {
		return s.Items.Type + "[]"
	}
	if s.Type == "" && s.Const != nil {
		return "string"
	}
	return s.Type
}

// rules is the compact constraint column: what a caller would otherwise have to
// read the whole description to find out.
func rules(s *oaSchema) string {
	if s == nil {
		return ""
	}
	var out []string
	// An array's permitted values live on its items, which is where the
	// per-deployment channel enum gets injected — reading only the field's own
	// enum would silently drop the one list a caller most needs to see.
	enum := s.Enum
	if len(enum) == 0 && s.Items != nil {
		enum = s.Items.Enum
	}
	if len(enum) > 0 {
		vals := make([]string, len(enum))
		for i, e := range enum {
			vals[i] = fmt.Sprint(e)
		}
		out = append(out, strings.Join(vals, " | "))
	}
	if s.Const != nil {
		out = append(out, fmt.Sprintf("always %v", s.Const))
	}
	if s.MaxBytes != nil {
		out = append(out, fmt.Sprintf("≤ %d bytes", *s.MaxBytes))
	}
	if s.MaxItems != nil {
		out = append(out, fmt.Sprintf("≤ %d items", *s.MaxItems))
	}
	if s.Items != nil && s.Items.MaxBytes != nil {
		out = append(out, fmt.Sprintf("each ≤ %d bytes", *s.Items.MaxBytes))
	}
	if s.Default != nil {
		out = append(out, fmt.Sprintf("default: %v", s.Default))
	}
	return strings.Join(out, " · ")
}

// deref resolves a local $ref. One level is all this spec uses, and a schema
// that refs a schema that refs another would be worth flattening in the
// document rather than chasing here.
func deref(s *oaSchema, spec *oaSpec) *oaSchema {
	if s == nil {
		return nil
	}
	if name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/"); ok {
		if target := spec.schema(name); target != nil {
			return target
		}
	}
	return s
}

// prettyJSON indents an example in place.
//
// json.Indent rather than a decode-and-re-encode round trip: unmarshalling into
// `any` turns an object into a map, and re-marshalling then emits the keys in
// sorted order — which would rewrite a hand-written example so that `message`
// came before `title`, and put that reordering straight into the textarea a
// reader is about to send.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return ""
	}
	return buf.String()
}

func methodRank(m string) int {
	if m == "POST" {
		return 0
	}
	return 1
}

func anchor(method, path string) string {
	s := strings.ToLower(method + path)
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
}

var (
	codeSpan = regexp.MustCompile("`([^`]+)`")
	emphasis = regexp.MustCompile(`\*([^*\n]+)\*`)
)

// inlineMarkdown renders the small slice of markdown the spec's prose actually
// uses — `code` and *emphasis*.
//
// Escaping first is what makes handing the result back as template.HTML safe:
// every byte that came from the document is inert text before any tag is
// introduced, so the only markup in the output is markup this function wrote.
func inlineMarkdown(s string) template.HTML {
	esc := template.HTMLEscapeString(s)
	esc = codeSpan.ReplaceAllString(esc, "<code>$1</code>")
	esc = emphasis.ReplaceAllString(esc, "<em>$1</em>")
	return template.HTML(esc)
}
