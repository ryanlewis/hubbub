package httpapi

import (
	"bytes"
	"embed"
	htmltmpl "html/template"
	"io"
	"net/http"
	"sync"
	texttmpl "text/template"
)

// The pages served to readers who arrive without a key. `theme.html` holds only
// the shared colour tokens, so a palette change lands on every page at once
// instead of being reapplied by hand to whichever ones someone remembers.
//
//go:embed index.html docs.html theme.html llms.txt
var pages embed.FS

// pageData is the only part of the landing page and llms.txt that cannot be
// written down at build time: where this hub is reached and which binary is
// answering. Both are properties of the deployment rather than of hubbub, which
// is the same reason the served OpenAPI spec injects them (see openapi.go).
type pageData struct {
	BaseURL string
	Version string
}

// Parsed on first use rather than in an init(), and deliberately without
// template.Must: these pages are cosmetic surface, and a bad edit to an
// embedded file must not be able to panic the process that carries the
// notification path. A parse failure costs the page and nothing else —
// TestLandingTemplatesParse is what keeps it out of a shipped binary.
var (
	htmlPages = sync.OnceValues(func() (*htmltmpl.Template, error) {
		return htmltmpl.ParseFS(pages, "theme.html", "index.html", "docs.html")
	})
	llmsTmpl = sync.OnceValues(func() (*texttmpl.Template, error) {
		return texttmpl.ParseFS(pages, "llms.txt")
	})
)

// handleIndex serves the human-readable landing page.
//
// Neither this nor handleLLMsTxt takes the *Server, and that is the point: they
// describe the API, never the deployment behind it. A handler with no access to
// the keyring, the channel set or the outbox is the cheapest possible guarantee
// that it cannot put any of them on a public page by accident. The docs page
// does take the server, because rendering the spec is exactly the job of
// reflecting this deployment — see docs.go.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, err := htmlPages()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "landing page unavailable")
		return
	}
	data := pageData{BaseURL: baseURL(r), Version: Version}
	renderPageTemplate(w, tmpl, "index.html", data, "text/html; charset=utf-8")
}

// handleLLMsTxt serves the llms.txt convention (llmstxt.org): the same contract
// as the landing page, condensed and in markdown, for an agent that was pointed
// at this host and has to work out what it can do here.
func handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	tmpl, err := llmsTmpl()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "llms.txt unavailable")
		return
	}
	data := pageData{BaseURL: baseURL(r), Version: Version}
	// text/plain rather than text/markdown: the file is markdown, but its whole
	// job is to render wherever it is fetched, and text/markdown makes some
	// browsers offer a download instead of showing it.
	renderPageTemplate(w, tmpl, "llms.txt", data, "text/plain; charset=utf-8")
}

// renderPageTemplate executes one named template from a set and writes it.
func renderPageTemplate(w http.ResponseWriter, tmpl interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}, name string, data any, contentType string) {
	// Rendered into a buffer rather than straight into the ResponseWriter: the
	// first byte written commits a 200, so a template error midway through would
	// hand back a truncated page that had already claimed success.
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		writeError(w, http.StatusInternalServerError, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
