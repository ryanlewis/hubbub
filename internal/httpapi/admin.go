package httpapi

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ryanlewis/hubbub/internal/adapter"
	"github.com/ryanlewis/hubbub/internal/adminauth"
	"github.com/ryanlewis/hubbub/internal/confedit"
	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/tomledit"
)

// Admin carries everything the dashboard needs. A nil *Admin on Server means
// no route is registered at all — the same presence-enabled shape as the ops
// listener and the heartbeat.
type Admin struct {
	Guard    *adminauth.Guard
	Keys     *confedit.File
	Channels *confedit.File
}

// keyPrefixLen is how much of a bearer key the dashboard will show. Enough to
// tell two keys apart in a list and to match one against a caller's own
// records; nowhere near enough to use.
const keyPrefixLen = 10

// maskedValue is what a credential's text becomes on screen.
const maskedValue = "••••••••"

// maskedSecret is that value as it appears in the settings editor, where it has
// to be a legal TOML string. Left unchanged on submit, it means "keep whatever
// is on disk".
const maskedSecret = `"` + maskedValue + `"`

// secretish reports whether a setting holds a credential.
//
// Erring towards masking is the right failure direction: masking a
// non-secret is a small annoyance, while missing one puts a live password in
// the DOM, in bfcache and in whatever the operator pastes into next — an
// exposure the 0600 file on disk never had.
func secretish(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"password", "token", "secret", "credential", "api_key", "apikey", "passphrase"} {
		if n == s || strings.HasSuffix(n, "_"+s) || strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// adminLink reports whether the dashboard belongs in this visitor's navigation.
//
// Deliberately the same question the guard would answer on /admin itself, asked
// early: a link that leads to a login redirect or a 403 is worse than no link,
// and on a hub reachable from the internet the bar would otherwise announce a
// dashboard to every anonymous reader. Safe on a nil *Admin, because the pages
// that call it are served whether or not one is configured.
func (s *Server) adminLink(r *http.Request) bool {
	if s.Admin == nil {
		return false
	}
	_, _, permitted := s.Admin.Guard.Identity(r)
	return permitted
}

// adminRoutes registers the dashboard. Everything is behind the guard, and
// every mutation is behind the CSRF check as well.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	// http.CrossOriginProtection rejects non-safe cross-origin browser
	// requests using Sec-Fetch-Site, falling back to Origin vs Host. The
	// visitor's browser is holding a proxy session cookie, so a form on
	// another site posting here is a real attack, not a theoretical one.
	csrf := http.NewCrossOriginProtection()

	protect := func(h http.HandlerFunc) http.Handler {
		return s.Admin.Guard.Protect(h)
	}
	mutate := func(h http.HandlerFunc) http.Handler {
		return s.Admin.Guard.Protect(csrf.Handler(h))
	}

	mux.Handle("GET /admin", protect(s.handleAdmin))
	mux.Handle("GET /admin/{$}", protect(s.handleAdmin))

	mux.Handle("POST /admin/channels", mutate(s.handleChannelCreate))
	mux.Handle("POST /admin/channels/{id}/toggle", mutate(s.handleChannelToggle))
	mux.Handle("POST /admin/channels/{id}/settings", mutate(s.handleChannelSettings))
	mux.Handle("POST /admin/channels/{id}/delete", mutate(s.handleChannelDelete))
	mux.Handle("POST /admin/channels/{id}/test", mutate(s.handleChannelTest))

	mux.Handle("POST /admin/callers", mutate(s.handleCallerCreate))
	mux.Handle("POST /admin/callers/{id}/grants", mutate(s.handleCallerGrants))
	mux.Handle("POST /admin/callers/{id}/rotate", mutate(s.handleCallerRotate))
	mux.Handle("POST /admin/callers/{id}/revoke", mutate(s.handleCallerRevoke))
	mux.Handle("POST /admin/callers/{id}/delete", mutate(s.handleCallerDelete))
}

// --- view model -------------------------------------------------------------

type adminView struct {
	Nonce      string
	Nav        navView
	Version    string
	Actor      string
	Channels   []adminChannel
	Callers    []adminCaller
	ChannelIDs []string
	Types      []string
	KeysETag   string
	ChansETag  string
	Notice     string
	Error      string
	NewKey     *newKeyView
	Confirm    *confirmView
	Editing    string
}

type adminChannel struct {
	ID      string
	Type    string
	Enabled bool
	Body    string // masked
}

type adminCaller struct {
	ID       string
	Keys     []adminKey
	Channels []string
	Grants   []adminGrant
}

type adminKey struct {
	Prefix string
	Full   string // only ever set on the page that just minted it
}

type adminGrant struct {
	Channel string
	Granted bool
}

type newKeyView struct {
	CallerID string
	Key      string
}

type confirmView struct {
	Title   string
	Action  string
	Fields  map[string]string
	Diff    []confedit.DiffLine
	Added   int
	Deleted int
}

// --- rendering --------------------------------------------------------------

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, v *adminView, status int) {
	tmpl, err := htmlPages()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "admin page unavailable")
		return
	}

	id, _, _ := s.Admin.Guard.Identity(r)
	v.Actor = id.Email
	v.Version = Version
	v.Nonce = rand.Text()
	// Not adminLink(r): reaching this render means the guard already let the
	// visitor through, so asking again could only ever disagree with itself.
	v.Nav = navView{Current: "/admin", Admin: true}
	if err := s.fillAdminState(v); err != nil {
		v.Error = strings.TrimSpace(v.Error + "\n" + err.Error())
	}

	// The dashboard displays live credentials and drives state changes, so it
	// gets the same locked-down policy as /docs plus the form-action it
	// actually needs. No third-party anything, and no framing.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; "+
		"script-src 'nonce-"+v.Nonce+"'; style-src 'nonce-"+v.Nonce+"'; "+
		"img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// A page listing key prefixes and channel credentials must not be held by
	// a shared cache, and must not come back from bfcache after a logout.
	w.Header().Set("Cache-Control", "no-store, private")

	renderPageTemplateStatus(w, tmpl, "admin.html", v, "text/html; charset=utf-8", status)
}

// fillAdminState reads both config files fresh. The files are the source of
// truth for the editor — the in-memory store lags by up to one poll, and it
// does not retain adapter settings at all.
func (s *Server) fillAdminState(v *adminView) error {
	chSrc, chETag, err := s.Admin.Channels.Read()
	if err != nil {
		return fmt.Errorf("reading channels: %w", err)
	}
	v.ChansETag = chETag

	live := s.Store.Channels()
	for _, id := range tomledit.IDs(chSrc) {
		c := adminChannel{ID: id}
		if lc, ok := live.Get(id); ok {
			c.Type, c.Enabled = lc.Type, lc.Enabled
		}
		if body, err := tomledit.Body(chSrc, id); err == nil {
			c.Body = string(maskSecrets(body))
		}
		v.Channels = append(v.Channels, c)
	}
	v.ChannelIDs = live.IDs()
	v.Types = adapter.Types()

	keySrc, keyETag, err := s.Admin.Keys.Read()
	if err != nil {
		return fmt.Errorf("reading keys: %w", err)
	}
	v.KeysETag = keyETag

	ring, err := config.ParseKeys(keySrc, s.Admin.Keys.Path)
	if err != nil {
		return fmt.Errorf("reading keys: %w", err)
	}
	for _, c := range ring.Callers() {
		ac := adminCaller{ID: c.ID, Channels: c.Channels}
		for _, k := range c.Keys {
			ac.Keys = append(ac.Keys, adminKey{Prefix: prefixOf(k)})
		}
		for _, ch := range v.ChannelIDs {
			ac.Grants = append(ac.Grants, adminGrant{Channel: ch, Granted: c.Permitted(ch)})
		}
		v.Callers = append(v.Callers, ac)
	}
	return nil
}

func prefixOf(key string) string {
	if len(key) <= keyPrefixLen {
		return key
	}
	return key[:keyPrefixLen] + "…"
}

// --- masking ----------------------------------------------------------------

// maskSecrets replaces credential values with a sentinel for display.
// Only string values are masked: a non-string setting is not a password, and
// replacing one would produce a body that no longer type-checks.
func maskSecrets(body []byte) []byte {
	out := make([]byte, 0, len(body))
	last := 0
	for _, k := range tomledit.KeyLines(body) {
		if !secretish(k.Name) || !quotedValue(k.Value) || k.Value == maskedSecret {
			continue
		}
		// k.End covers the whole assignment, so a value written across several
		// lines collapses to this one masked line. Replacing only its first line
		// would leave the rest of the secret in the textarea *and* leave behind a
		// dangling `"""` that no longer parses.
		out = append(out, body[last:k.Start]...)
		out = append(out, []byte(k.Name+" = "+maskedSecret+"\n")...)
		last = k.End
	}
	return append(out, body[last:]...)
}

// quotedValue reports whether a raw TOML value is a string in any of the four
// spellings. Matching only `"` left `password = 'app-secret'` in the DOM, and
// counted a `"""` multi-line secret as maskable when only its opening delimiter
// was on the line being replaced.
func quotedValue(raw string) bool {
	return strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, `'`)
}

// lineMask decides what one value in a config line looks like on screen.
type lineMask func(name, raw string) string

// maskFor picks the mask a file's own text must pass through before a browser
// sees it.
//
// It asks which file rather than taking an argument so that the strict mask is
// what a caller gets by default: a preview added later for a file nobody
// considered here is over-masked at worst, instead of shipping credentials to
// the DOM because a parameter was forgotten.
func (s *Server) maskFor(f *confedit.File) lineMask {
	if f == s.Admin.Channels {
		return maskChannelSetting
	}
	return maskCallerValue
}

// maskChannelSetting blanks the credentials in channels.toml. Everything else
// in that file — servers, topics, from addresses — is what the operator opened
// the page to read, so only a secretish name is touched.
func maskChannelSetting(name, raw string) string {
	if raw == "" || !secretish(name) {
		return raw
	}
	return maskedValue
}

// maskCallerValue cuts a value in keys.toml down to what the caller list
// already shows.
//
// The rule is inverted here because the file is nothing but credentials: mask
// everything except the fields that demonstrably are not, and treat a value no
// field name could be attached to as a credential as well. A hand-written
// multi-line `key = [` array is exactly that case, and "I could not tell what
// this line was" must never resolve to "so here is the key".
func maskCallerValue(name, raw string) string {
	switch name {
	case "channels", "defaults":
		return raw
	}
	if raw == "" {
		return raw
	}
	// A prefix is what makes a diff checkable — it matches the line against the
	// row that asked for the change, which is the whole point of confirming.
	// Anything short enough that a prefix would give most of it away is blanked
	// instead; a 16-byte key is a poor key, but it is still a live one.
	if len(raw) > keyPrefixLen*2 {
		return prefixOf(raw)
	}
	return maskedValue
}

// maskDiff redacts a diff for display.
//
// Deliberately after Diff, never before it: masking the two files first would
// make a changed password diff as no change at all, and the operator would be
// told there was nothing to do while their edit sat unapplied. Diffing the real
// bytes and masking only the rendering keeps the line counts honest.
func maskDiff(d []confedit.DiffLine, mask lineMask) []confedit.DiffLine {
	out := make([]confedit.DiffLine, len(d))
	for i, l := range d {
		out[i] = confedit.DiffLine{Op: l.Op, Text: string(tomledit.MaskStrings([]byte(l.Text), mask))}
	}
	return out
}

// unmaskSecrets puts real values back where the operator left the mask alone.
func unmaskSecrets(submitted, original []byte) ([]byte, error) {
	originals := map[string]string{}
	for _, k := range tomledit.KeyLines(original) {
		originals[k.Name] = k.Value
	}

	out := make([]byte, 0, len(submitted))
	last := 0
	for _, k := range tomledit.KeyLines(submitted) {
		if k.Value != maskedSecret {
			continue
		}
		real, ok := originals[k.Name]
		if !ok {
			return nil, fmt.Errorf("%s was left as %s but has no value on disk to keep — type a real one", k.Name, maskedSecret)
		}
		out = append(out, submitted[last:k.Start]...)
		out = append(out, []byte(k.Name+" = "+real+"\n")...)
		last = k.End
	}
	return append(out, submitted[last:]...), nil
}

// --- helpers ----------------------------------------------------------------

// audit records a change and makes it live. Both are unconditional: a change
// nobody can attribute later is barely better than no change control at all.
func (s *Server) audit(r *http.Request, action, target string) {
	id, _, _ := s.Admin.Guard.Identity(r)
	s.Log.Append(dlog.Record{
		Kind:   "admin",
		Actor:  id.Email,
		Action: action,
		Detail: target,
	})
	s.Metrics.AdminChange(action)
	s.Store.ReloadNow()
}

// fail re-renders the dashboard with an error. The status is carried so a
// conflict reads as a conflict to anything scripting this.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, confedit.ErrConflict) {
		status = http.StatusConflict
		err = fmt.Errorf("%w — someone edited it over SSH, or another tab saved first. Reload and reapply your change", err)
	}
	s.renderAdmin(w, r, &adminView{Error: err.Error()}, status)
}

func (s *Server) ok(w http.ResponseWriter, r *http.Request, notice string) {
	s.renderAdmin(w, r, &adminView{Notice: notice}, http.StatusOK)
}

// tomlString quotes a value for writing into a config file.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlStringArray(xs []string) string {
	quoted := make([]string, 0, len(xs))
	for _, x := range xs {
		quoted = append(quoted, tomlString(x))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// newKey mints a bearer key. rand.Text is the stdlib's own
// "give me an unguessable string" and yields ~130 bits.
func newKey() string {
	return "nh_" + strings.ToLower(rand.Text())
}
