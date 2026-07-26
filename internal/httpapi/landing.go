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
// `chrome.html` is the same bargain for the masthead, markup and style
// together: the bar is identical on all three pages or on none, and a link
// added to it appears everywhere.
//
//go:embed index.html docs.html admin.html chrome.html theme.html llms.txt
var pages embed.FS

// Embedded separately from pages: it is served as bytes, never templated, and
// putting it in the FS as well would carry a second copy in the binary.
//
//go:embed favicon.svg
var faviconSVG []byte

// pageData is the only part of the landing page and llms.txt that cannot be
// written down at build time: where this hub is reached and which binary is
// answering. Both are properties of the deployment rather than of hubbub, which
// is the same reason the served OpenAPI spec injects them (see openapi.go).
type pageData struct {
	BaseURL string
	Version string
	Chrome  chromeView
}

// chromeView is the masthead every browser page wears. The bar itself is one
// piece of markup in one place (chrome.html); what differs between pages is
// data, so no page can quietly grow a header of its own.
type chromeView struct {
	Nav     navView
	Version string
	// Actor is the operator the proxy says is signed in — the dashboard only.
	// The other two pages are readable by anyone who can reach the hub, so
	// there is nobody to name.
	//
	// It is the only thing a page may put in the bar. Anything else a page
	// wants to say goes underneath it, as content: a bar that is a different
	// height on one page is the same flinch this type exists to remove.
	Actor string
}

// navView is the site navigation, shared by the three pages a browser lands on.
//
// The dashboard link is per-visitor rather than per-deployment: a hub with no
// [admin] block does not serve /admin at all, and one that does still refuses
// everybody but the allowlist. Offering the link to a reader who would be
// bounced is a dead end that also tells the internet the dashboard is there.
type navView struct {
	Current string // the page being rendered, marked as current in the bar
	Admin   bool   // whether this visitor is offered the dashboard
}

// Parsed on first use rather than in an init(), and deliberately without
// template.Must: these pages are cosmetic surface, and a bad edit to an
// embedded file must not be able to panic the process that carries the
// notification path. A parse failure costs the page and nothing else —
// TestLandingTemplatesParse is what keeps it out of a shipped binary.
var (
	htmlPages = sync.OnceValues(func() (*htmltmpl.Template, error) {
		return htmltmpl.ParseFS(pages, "theme.html", "chrome.html", "index.html", "docs.html", "admin.html")
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
//
// adminLink is the one thing the page needs that only the server knows, and it
// is taken as a callback returning a single bool rather than as the server
// itself: the guarantee above is worth more than the argument it saves.
func handleIndex(adminLink func(*http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tmpl, err := htmlPages()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "landing page unavailable")
			return
		}
		data := pageData{
			BaseURL: baseURL(r),
			Version: Version,
			Chrome: chromeView{
				Nav:     navView{Current: "/", Admin: adminLink(r)},
				Version: Version,
			},
		}
		renderPageTemplate(w, tmpl, "index.html", data, "text/html; charset=utf-8")
	}
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

// handleFavicon serves the tab icon.
//
// SVG only, and no /favicon.ico: every browser that has shipped in the last few
// years takes the <link> to an SVG, and the alternative is either a
// hand-assembled ICO container or a second copy of the mark drawn again in Go
// to rasterise — two descriptions of one icon, which is the shape of thing that
// drifts. A client old enough to ignore it falls back to a blank tab, which is
// where this started.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	// Fixed for the life of the binary and re-requested on every page load and
	// tab restore — the one asset here where revalidation is pure waste.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconSVG)
}

// renderPageTemplate executes one named template from a set and writes it.
func renderPageTemplate(w http.ResponseWriter, tmpl interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}, name string, data any, contentType string) {
	renderPageTemplateStatus(w, tmpl, name, data, contentType, http.StatusOK)
}

// renderPageTemplateStatus is renderPageTemplate with the status spelled out,
// for pages that answer a failed form post with the page itself.
func renderPageTemplateStatus(w http.ResponseWriter, tmpl interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}, name string, data any, contentType string, status int) {
	// Rendered into a buffer rather than straight into the ResponseWriter: the
	// first byte written commits a 200, so a template error midway through would
	// hand back a truncated page that had already claimed success.
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		writeError(w, http.StatusInternalServerError, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
