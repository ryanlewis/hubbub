package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlewis/hubbub/internal/adminauth"
	"github.com/ryanlewis/hubbub/internal/confedit"
	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
)

const adminEmail = "ryan@rlew.io"

// adminServer wires the dashboard over real files on disk, because every
// interesting property here is about what ends up in those files.
func adminServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	s := newTestServer(t, "http://127.0.0.1:1", "")

	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.toml")
	chansPath := filepath.Join(dir, "channels.toml")

	// Shaped like the shipped examples: a file preamble, then a table, then
	// trailing documentation. The documentation is what a bad boundary rule
	// eats, so it has to be here.
	if err := os.WriteFile(keysPath, []byte(`# Preamble that belongs to the file.
# Second preamble line.

[dev]
key = "nh_test_key_0123456789"
channels = ["ntfy"]

# How to rotate:
# add the new key, flip the caller over, then remove the old one.
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chansPath, []byte(`# Channel instances.

[ntfy]
type = "ntfy"
server = "http://127.0.0.1:1"
# The topic is the secret on a public instance.
topic = "tst"
token = "s3cret-token"

# Parked example, kept as documentation.
#
# [standby]
# type = "ntfy"
# enabled = false
`), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := config.NewStore(&config.Config{KeysFile: keysPath, ChannelsFile: chansPath})
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store

	// Own delivery log, next to the config, so the audit assertions have a
	// path they can read back.
	logger, err := dlog.Open(filepath.Join(dir, "delivery.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logger.Close() })
	s.Log = logger

	provider, err := adminauth.New("exe-dev")
	if err != nil {
		t.Fatal(err)
	}
	guard, err := adminauth.NewGuard(provider, []string{adminEmail})
	if err != nil {
		t.Fatal(err)
	}
	s.Admin = &Admin{
		Guard: guard,
		Keys: &confedit.File{Path: keysPath, Validate: func(src []byte, name string) error {
			_, err := config.ParseKeys(src, name)
			return err
		}},
		Channels: &confedit.File{Path: chansPath, Validate: func(src []byte, name string) error {
			_, err := config.ParseChannels(src, name)
			return err
		}},
	}
	return s, keysPath, chansPath
}

func adminGet(t *testing.T, s *Server, path, email string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if email != "" {
		req.Header.Set("X-ExeDev-Email", email)
	}
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)
	return rec
}

func adminPost(t *testing.T, s *Server, path, email string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if email != "" {
		req.Header.Set("X-ExeDev-Email", email)
	}
	// Same-origin, as a browser submitting the page's own form would report.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)
	return rec
}

func etags(t *testing.T, s *Server) (keys, chans string) {
	t.Helper()
	_, k, err := s.Admin.Keys.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, c, err := s.Admin.Channels.Read()
	if err != nil {
		t.Fatal(err)
	}
	return k, c
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// readLog parses the delivery log that sits beside the config files. Asserting
// on the JSONL rather than on internals is this repo's convention for outbox
// behaviour, and an audit trail is worth holding to the same standard.
func readLog(t *testing.T, keysPath string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(keysPath), "delivery.log"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// --- gating -----------------------------------------------------------------

func TestAdminRoutesAbsentWithoutConfig(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", "")
	if s.Admin != nil {
		t.Fatal("test server should start without a dashboard")
	}
	if rec := adminGet(t, s, "/admin", adminEmail); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when [admin] is absent", rec.Code)
	}
}

// An anonymous request must be bounced *and* must not disclose anything about
// the deployment on the way out.
func TestAnonymousIsBouncedAndLeaksNothing(t *testing.T) {
	s, _, _ := adminServer(t)

	paths := []string{"/admin", "/admin/"}
	for _, p := range paths {
		rec := adminGet(t, s, p, "")
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status = %d, want 302", p, rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "__exe.dev/login") {
			t.Errorf("%s: Location = %q", p, loc)
		}
		body := rec.Body.String()
		// "dev" is deliberately not in this list: it occurs in "__exe.dev" in
		// the redirect itself. These are the tokens that could only come from
		// the deployment's own config.
		for _, secret := range []string{"nh_test_key", "s3cret-token", "topic", "[dev]", "channels ="} {
			if strings.Contains(body, secret) {
				t.Errorf("%s: redirect body leaked %q: %q", p, secret, body)
			}
		}
		if len(body) > 200 {
			t.Errorf("%s: redirect body is %d bytes, want a bare redirect stub", p, len(body))
		}
	}
}

func TestNonAllowlistedIsRefused(t *testing.T) {
	s, _, _ := adminServer(t)
	rec := adminGet(t, s, "/admin", "someone@example.com")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "someone@example.com") {
		t.Error("the 403 does not name the refused address")
	}
	if strings.Contains(rec.Body.String(), "nh_test_key") {
		t.Error("the 403 leaked a key")
	}
}

func TestEveryMutationRequiresIdentity(t *testing.T) {
	s, keysPath, chansPath := adminServer(t)
	beforeKeys, beforeChans := read(t, keysPath), read(t, chansPath)

	for _, p := range []string{
		"/admin/channels",
		"/admin/channels/ntfy/toggle",
		"/admin/channels/ntfy/settings",
		"/admin/channels/ntfy/delete",
		"/admin/channels/ntfy/test",
		"/admin/callers",
		"/admin/callers/dev/grants",
		"/admin/callers/dev/rotate",
		"/admin/callers/dev/revoke",
		"/admin/callers/dev/delete",
	} {
		rec := adminPost(t, s, p, "", url.Values{"confirm": {"1"}})
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status = %d, want a 302 to login", p, rec.Code)
		}
	}
	if read(t, keysPath) != beforeKeys || read(t, chansPath) != beforeChans {
		t.Error("an unauthenticated request changed a config file")
	}
}

// The visitor's browser holds a proxy session cookie, so a form on another
// site posting here is a real attack.
func TestCrossOriginMutationIsRejected(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	before := read(t, keysPath)

	req := httptest.NewRequest(http.MethodPost, "/admin/callers", strings.NewReader("id=evil"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ExeDev-Email", adminEmail)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.PublicMux().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("a cross-site POST was accepted (status %d)", rec.Code)
	}
	if read(t, keysPath) != before {
		t.Error("a cross-site POST changed keys.toml")
	}
}

// --- callers ----------------------------------------------------------------

func TestCreateCallerShowsKeyOnceThenOnlyAPrefix(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	k, _ := etags(t, s)

	rec := adminPost(t, s, "/admin/callers", adminEmail, url.Values{"id": {"digest"}, "etag": {k}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// The key is on the creating response exactly once.
	body := rec.Body.String()
	start := strings.Index(body, "nh_")
	if start < 0 {
		t.Fatalf("the new key was not shown: %s", body)
	}
	end := start
	for end < len(body) && (body[end] == '_' || (body[end] >= 'a' && body[end] <= 'z') || (body[end] >= '0' && body[end] <= '9')) {
		end++
	}
	key := body[start:end]
	if len(key) < 20 {
		t.Fatalf("generated key looks too short: %q", key)
	}
	if strings.Count(body, key) != 1 {
		t.Errorf("the key appears %d times on the page, want exactly once", strings.Count(body, key))
	}
	if !strings.Contains(read(t, keysPath), key) {
		t.Error("the generated key was not written to keys.toml")
	}

	// And never again.
	next := adminGet(t, s, "/admin", adminEmail)
	if strings.Contains(next.Body.String(), key) {
		t.Error("the full key was shown again on a later render")
	}
	if !strings.Contains(next.Body.String(), key[:keyPrefixLen]) {
		t.Error("the key prefix is not shown, so keys cannot be told apart")
	}

	// Deny by default: a new caller reaches nothing.
	ring, err := config.ParseKeys([]byte(read(t, keysPath)), "k")
	if err != nil {
		t.Fatalf("keys.toml no longer loads: %v", err)
	}
	c, ok := findCaller(ring, "digest")
	if !ok {
		t.Fatal("caller not created")
	}
	if len(c.Channels) != 0 {
		t.Errorf("new caller was granted %v, want nothing", c.Channels)
	}
}

// The id is interpolated into a `[<id>]` header as raw text, so it has to be
// validated before it gets there: a newline splices in a whole second caller
// table — with a key the submitter chose — and the result parses perfectly and
// audits as one create.
func TestCreateCallerRejectsUnsafeIDs(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	before := read(t, keysPath)

	injection := "evil]\nkey = \"nh_attacker_chosen_key_0123456789\"\nchannels = [\"ntfy\"]\n[decoy"
	for _, id := range []string{injection, "ops.team", "a b", "", strings.Repeat("x", 65)} {
		k, _ := etags(t, s)
		rec := adminPost(t, s, "/admin/callers", adminEmail, url.Values{"etag": {k}, "id": {id}})
		if rec.Code == http.StatusOK {
			t.Errorf("id %q was accepted", id)
		}
	}
	if read(t, keysPath) != before {
		t.Error("keys.toml changed despite refused creates")
	}
	ring, err := config.ParseKeys([]byte(read(t, keysPath)), "k")
	if err != nil {
		t.Fatalf("keys.toml no longer loads: %v", err)
	}
	if _, ok := ring.Lookup("nh_attacker_chosen_key_0123456789"); ok {
		t.Error("an injected key authenticates")
	}
}

func TestCreateCallerPreservesComments(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	k, _ := etags(t, s)
	adminPost(t, s, "/admin/callers", adminEmail, url.Values{"id": {"digest"}, "etag": {k}})

	after := read(t, keysPath)
	for _, want := range []string{"# Preamble that belongs to the file.", "# How to rotate:"} {
		if !strings.Contains(after, want) {
			t.Errorf("creating a caller deleted %q\n%s", want, after)
		}
	}
}

func TestRotateAddsASecondKeyAndRevokeRemovesIt(t *testing.T) {
	s, keysPath, _ := adminServer(t)

	k, _ := etags(t, s)
	rec := adminPost(t, s, "/admin/callers/dev/rotate", adminEmail, url.Values{"etag": {k}})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	ring, err := config.ParseKeys([]byte(read(t, keysPath)), "k")
	if err != nil {
		t.Fatalf("keys.toml no longer loads: %v", err)
	}
	c, _ := findCaller(ring, "dev")
	if len(c.Keys) != 2 {
		t.Fatalf("after rotate the caller has %d keys, want 2 — rotation must not cause an outage", len(c.Keys))
	}
	// Both keys authenticate during the overlap.
	for _, key := range c.Keys {
		if _, ok := ring.Lookup(key); !ok {
			t.Errorf("key %s does not authenticate mid-rotation", prefixOf(key))
		}
	}

	// Revoke the original, with the confirm step.
	old := prefixOf("nh_test_key_0123456789")
	k, _ = etags(t, s)
	rec = adminPost(t, s, "/admin/callers/dev/revoke", adminEmail, url.Values{"etag": {k}, "prefix": {old}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Confirm") {
		t.Fatalf("revoke did not ask for confirmation: %d", rec.Code)
	}
	if len(mustCaller(t, keysPath, "dev").Keys) != 2 {
		t.Fatal("the preview step already applied the change")
	}

	k, _ = etags(t, s)
	rec = adminPost(t, s, "/admin/callers/dev/revoke", adminEmail, url.Values{"etag": {k}, "prefix": {old}, "confirm": {"1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke apply: %d %s", rec.Code, rec.Body.String())
	}
	after := mustCaller(t, keysPath, "dev")
	if len(after.Keys) != 1 {
		t.Fatalf("after revoke the caller has %d keys, want 1", len(after.Keys))
	}
	if after.Keys[0] == "nh_test_key_0123456789" {
		t.Error("the wrong key was revoked")
	}
}

// A caller with no key at all fails the load, which would take every other
// caller down with it.
func TestRevokingTheOnlyKeyIsRefused(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	k, _ := etags(t, s)
	rec := adminPost(t, s, "/admin/callers/dev/revoke", adminEmail, url.Values{
		"etag": {k}, "prefix": {prefixOf("nh_test_key_0123456789")}, "confirm": {"1"},
	})
	if rec.Code == http.StatusOK {
		t.Error("revoking the last key was allowed")
	}
	if !strings.Contains(rec.Body.String(), "only key") {
		t.Errorf("the refusal does not explain itself: %s", rec.Body.String())
	}
	if _, err := config.ParseKeys([]byte(read(t, keysPath)), "k"); err != nil {
		t.Errorf("keys.toml was left unloadable: %v", err)
	}
}

func TestGrantsRoundTrip(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	k, _ := etags(t, s)

	rec := adminPost(t, s, "/admin/callers/dev/grants", adminEmail, url.Values{"etag": {k}})
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing grants: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustCaller(t, keysPath, "dev").Channels; len(got) != 0 {
		t.Errorf("channels = %v, want empty", got)
	}

	k, _ = etags(t, s)
	rec = adminPost(t, s, "/admin/callers/dev/grants", adminEmail, url.Values{"etag": {k}, "channel": {"ntfy"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("granting: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustCaller(t, keysPath, "dev").Channels; len(got) != 1 || got[0] != "ntfy" {
		t.Errorf("channels = %v, want [ntfy]", got)
	}
}

// A grant naming a channel that does not exist is dead config that looks live.
func TestGrantingAnUnknownChannelIsRefused(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	before := read(t, keysPath)
	k, _ := etags(t, s)

	rec := adminPost(t, s, "/admin/callers/dev/grants", adminEmail, url.Values{"etag": {k}, "channel": {"nope"}})
	if rec.Code == http.StatusOK {
		t.Error("a grant to a nonexistent channel was accepted")
	}
	if read(t, keysPath) != before {
		t.Error("keys.toml changed despite a refused grant")
	}
}

func mustCaller(t *testing.T, keysPath, id string) *config.Caller {
	t.Helper()
	ring, err := config.ParseKeys([]byte(read(t, keysPath)), "k")
	if err != nil {
		t.Fatalf("keys.toml no longer loads: %v", err)
	}
	c, ok := findCaller(ring, id)
	if !ok {
		t.Fatalf("no caller %q", id)
	}
	return c
}

// --- channels ---------------------------------------------------------------

func TestToggleChannelKeepsCommentsAndReloads(t *testing.T) {
	s, _, chansPath := adminServer(t)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/toggle", adminEmail, url.Values{"etag": {c}, "enable": {"0"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: %d %s", rec.Code, rec.Body.String())
	}
	after := read(t, chansPath)

	set, err := config.ParseChannels([]byte(after), "c")
	if err != nil {
		t.Fatalf("channels.toml no longer loads: %v", err)
	}
	ch, _ := set.Get("ntfy")
	if ch.Enabled {
		t.Error("the channel is still enabled")
	}
	for _, want := range []string{"# Channel instances.", "# [standby]", "# The topic is the secret on a public instance."} {
		if !strings.Contains(after, want) {
			t.Errorf("toggling deleted %q\n%s", want, after)
		}
	}
	if !strings.Contains(rec.Body.String(), "spool is kept") {
		t.Error("disabling did not explain that the spool survives")
	}
}

// The whole point of the settings editor: a credential must not reach the DOM.
func TestSettingsEditorMasksSecretsAndKeepsThemOnSave(t *testing.T) {
	s, _, chansPath := adminServer(t)

	page := adminGet(t, s, "/admin?edit=ntfy", adminEmail)
	if strings.Contains(page.Body.String(), "s3cret-token") {
		t.Error("the settings editor rendered a live credential")
	}
	if !strings.Contains(page.Body.String(), "••••••••") {
		t.Error("the credential was not masked")
	}

	// Save with the mask untouched: the real value must survive.
	_, c := etags(t, s)
	body := "type = \"ntfy\"\r\nserver = \"http://127.0.0.1:1\"\r\ntopic = \"changed\"\r\ntoken = \"••••••••\"\r\n"
	rec := adminPost(t, s, "/admin/channels/ntfy/settings", adminEmail, url.Values{"etag": {c}, "body": {body}, "confirm": {"1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	after := read(t, chansPath)
	if !strings.Contains(after, `token = "s3cret-token"`) {
		t.Errorf("the masked credential was not preserved:\n%s", after)
	}
	if !strings.Contains(after, `topic = "changed"`) {
		t.Errorf("the edit did not land:\n%s", after)
	}
	// A textarea always submits CRLF; an LF file must stay LF.
	if strings.Contains(after, "\r") {
		t.Error("CRLF from the form leaked into an LF file")
	}
}

// TOML spells a string four ways and only one of them was masked. A literal
// 'single-quoted' secret went to the browser verbatim; a """ multi-line one was
// worse — the opening delimiter looked maskable, so the first line was replaced
// and the secret's own lines were left behind, both leaking it and leaving a body
// that no longer parsed.
func TestSettingsEditorMasksEveryStringForm(t *testing.T) {
	for _, tc := range []struct {
		name, assignment, secret string
	}{
		{"literal", "token = 'literal-s3cret'", "literal-s3cret"},
		{"multiline", "token = \"\"\"\nmultiline-s3cret\n\"\"\"", "multiline-s3cret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, chansPath := adminServer(t)
			body := "# Channel instances.\n\n[ntfy]\ntype = \"ntfy\"\nserver = \"http://127.0.0.1:1\"\ntopic = \"tst\"\n" +
				tc.assignment + "\n\n# Trailing documentation.\n"
			if err := os.WriteFile(chansPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			page := adminGet(t, s, "/admin?edit=ntfy", adminEmail).Body.String()
			if strings.Contains(page, tc.secret) {
				t.Errorf("the settings editor rendered a live credential")
			}
			if !strings.Contains(page, "••••••••") {
				t.Error("the credential was not masked")
			}

			// And the mask still means "keep what's on disk" on the way back.
			_, c := etags(t, s)
			submitted := "type = \"ntfy\"\r\nserver = \"http://127.0.0.1:1\"\r\ntopic = \"changed\"\r\ntoken = \"••••••••\"\r\n"
			rec := adminPost(t, s, "/admin/channels/ntfy/settings", adminEmail,
				url.Values{"etag": {c}, "body": {submitted}, "confirm": {"1"}})
			if rec.Code != http.StatusOK {
				t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
			}
			after := read(t, chansPath)
			if !strings.Contains(after, tc.secret) {
				t.Errorf("the masked credential was not preserved:\n%s", after)
			}
			if !strings.Contains(after, `topic = "changed"`) {
				t.Errorf("the edit did not land:\n%s", after)
			}
			if _, err := config.ParseChannels([]byte(after), "c"); err != nil {
				t.Errorf("channels.toml no longer loads: %v\n%s", err, after)
			}
			if !strings.Contains(after, "# Trailing documentation.") {
				t.Errorf("the save ate a comment:\n%s", after)
			}
		})
	}
}

func TestSettingsEditorRefusesATableHeader(t *testing.T) {
	s, _, chansPath := adminServer(t)
	before := read(t, chansPath)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/settings", adminEmail, url.Values{
		"etag": {c}, "confirm": {"1"},
		"body": {"[ntfy2]\ntype = \"ntfy\"\ntopic = \"t\"\n"},
	})
	if rec.Code == http.StatusOK {
		t.Error("a renamed table header was accepted — that would orphan the spool")
	}
	if read(t, chansPath) != before {
		t.Error("channels.toml changed despite a refused edit")
	}
}

func TestInvalidSettingsAreRefusedWithTheLoadersError(t *testing.T) {
	s, _, chansPath := adminServer(t)
	before := read(t, chansPath)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/settings", adminEmail, url.Values{
		"etag": {c}, "confirm": {"1"},
		"body": {"type = \"ntfy\"\ntopic = \"t\"\nprot = 587\n"},
	})
	if rec.Code == http.StatusOK {
		t.Fatal("an unknown setting was accepted")
	}
	if !strings.Contains(rec.Body.String(), "prot") {
		t.Errorf("the loader's own error was not surfaced: %s", rec.Body.String())
	}
	if read(t, chansPath) != before {
		t.Error("channels.toml changed despite a refused edit")
	}
}

// Nothing structural can see comment loss, so the diff is the last line of
// defence — and it has to actually appear before a freeform save lands.
func TestSettingsSaveShowsADiffFirst(t *testing.T) {
	s, _, chansPath := adminServer(t)
	before := read(t, chansPath)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/settings", adminEmail, url.Values{
		"etag": {c},
		"body": {"type = \"ntfy\"\nserver = \"http://127.0.0.1:1\"\ntopic = \"different\"\ntoken = \"••••••••\"\n"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Confirm") || !strings.Contains(body, "different") {
		t.Errorf("no diff was shown: %s", body)
	}
	if read(t, chansPath) != before {
		t.Error("the preview step already wrote the change")
	}
}

func TestCreateChannelLandsDisabled(t *testing.T) {
	s, _, chansPath := adminServer(t)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels", adminEmail, url.Values{"etag": {c}, "id": {"pager"}, "type": {"ntfy"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	set, err := config.ParseChannels([]byte(read(t, chansPath)), "c")
	if err != nil {
		t.Fatalf("channels.toml no longer loads: %v", err)
	}
	ch, ok := set.Get("pager")
	if !ok {
		t.Fatal("channel not created")
	}
	// An adapter validates at construction, so a channel with no settings
	// could only ever load while parked.
	if ch.Enabled {
		t.Error("a channel with no settings was created enabled")
	}
}

func TestCreateChannelRejectsUnsafeIDs(t *testing.T) {
	s, _, chansPath := adminServer(t)
	before := read(t, chansPath)

	for _, id := range []string{"..", "a/b", "", strings.Repeat("x", 65)} {
		_, c := etags(t, s)
		rec := adminPost(t, s, "/admin/channels", adminEmail, url.Values{"etag": {c}, "id": {id}, "type": {"ntfy"}})
		if rec.Code == http.StatusOK {
			t.Errorf("id %q was accepted — it names a spool directory", id)
		}
	}
	if read(t, chansPath) != before {
		t.Error("channels.toml changed despite refused creates")
	}
}

func TestDeleteChannelWarnsAboutGrantsAndSpool(t *testing.T) {
	s, _, chansPath := adminServer(t)
	before := read(t, chansPath)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/delete", adminEmail, url.Values{"etag": {c}})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete preview: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "spool") {
		t.Error("the confirmation does not mention that the spool is destroyed")
	}
	if !strings.Contains(body, "dev") {
		t.Error("the confirmation does not name the callers still granted the channel")
	}
	if read(t, chansPath) != before {
		t.Error("the preview step already deleted the channel")
	}
}

func TestDeleteChannelPreservesTrailingDocumentation(t *testing.T) {
	s, _, chansPath := adminServer(t)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/delete", adminEmail, url.Values{"etag": {c}, "confirm": {"1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	after := read(t, chansPath)
	for _, want := range []string{"# Channel instances.", "# [standby]", "# Parked example, kept as documentation."} {
		if !strings.Contains(after, want) {
			t.Errorf("deleting a channel ate %q\n%s", want, after)
		}
	}
	if strings.Contains(after, "s3cret-token") {
		t.Error("the channel body was left behind")
	}
}

// --- concurrency ------------------------------------------------------------

func TestStaleETagIsAConflict(t *testing.T) {
	s, _, chansPath := adminServer(t)
	_, c := etags(t, s)

	// Somebody edits over SSH while the page is open.
	current := read(t, chansPath)
	if err := os.WriteFile(chansPath, []byte(current+"\n# a hand edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := adminPost(t, s, "/admin/channels/ntfy/toggle", adminEmail, url.Values{"etag": {c}, "enable": {"0"}})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	after := read(t, chansPath)
	if !strings.Contains(after, "# a hand edit") {
		t.Error("the hand edit was destroyed")
	}
}

// --- audit ------------------------------------------------------------------

func TestEveryMutationWritesAnAuditLine(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	k, c := etags(t, s)

	adminPost(t, s, "/admin/channels/ntfy/toggle", adminEmail, url.Values{"etag": {c}, "enable": {"0"}})
	adminPost(t, s, "/admin/callers", adminEmail, url.Values{"etag": {k}, "id": {"digest"}})

	lines := readLog(t, keysPath)
	var admins []map[string]any
	for _, l := range lines {
		if l["kind"] == "admin" {
			admins = append(admins, l)
		}
	}
	if len(admins) != 2 {
		t.Fatalf("got %d admin log lines, want 2: %v", len(admins), lines)
	}
	for _, l := range admins {
		if l["actor"] != adminEmail {
			t.Errorf("actor = %v, want the operator's address", l["actor"])
		}
		if l["action"] == "" || l["action"] == nil {
			t.Error("no action recorded")
		}
	}
	if got := s.Metrics.Render(); !strings.Contains(got, "notify_admin_changes_total") {
		t.Errorf("no admin metric emitted:\n%s", got)
	}
}

// --- leak checks ------------------------------------------------------------

// The dashboard now shares a listener with the public pages, so the existing
// leak guarantees have to be re-checked with it switched on.
func TestPublicPagesLeakNothingWithTheDashboardEnabled(t *testing.T) {
	s, _, _ := adminServer(t)
	for _, p := range []string{"/", "/llms.txt", "/openapi.json", "/docs"} {
		rec := adminGet(t, s, p, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d", p, rec.Code)
			continue
		}
		body := rec.Body.String()
		for _, secret := range []string{"nh_test_key", "s3cret-token", adminEmail, "/admin"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked %q", p, secret)
			}
		}
	}
}

// diffOf pulls out the rendered diff, so an assertion about what the diff shows
// cannot be satisfied by the confirmation's own prose above it.
func diffOf(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<div class="diff">`)
	if start < 0 {
		t.Fatalf("no diff on the page:\n%s", body)
	}
	end := strings.Index(body[start:], "</pre>")
	if end < 0 {
		t.Fatalf("unterminated diff:\n%s", body[start:])
	}
	return body[start : start+end]
}

// The confirmation diff is raw config text, and keys.toml is nothing but
// credentials — including those of callers the operator is not touching, which
// appear as unchanged context lines.
func TestConfirmationDiffNeverShowsAKey(t *testing.T) {
	s, keysPath, _ := adminServer(t)
	const devKey = "nh_test_key_0123456789"

	// A caller nobody is editing: its key is context in every diff of this file.
	k, _ := etags(t, s)
	adminPost(t, s, "/admin/callers", adminEmail, url.Values{"etag": {k}, "id": {"bystander"}})
	bystander := mustCaller(t, keysPath, "bystander").Keys[0]

	// Rotate so there are two keys and revoking one is legal.
	k, _ = etags(t, s)
	adminPost(t, s, "/admin/callers/dev/rotate", adminEmail, url.Values{"etag": {k}})
	var rotated string
	for _, key := range mustCaller(t, keysPath, "dev").Keys {
		if key != devKey {
			rotated = key
		}
	}
	if rotated == "" {
		t.Fatal("rotate did not add a second key")
	}

	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/admin/callers/dev/revoke", url.Values{"prefix": {prefixOf(devKey)}}},
		{"/admin/callers/dev/delete", url.Values{}},
	} {
		k, _ = etags(t, s)
		tc.form.Set("etag", k)
		rec := adminPost(t, s, tc.path, adminEmail, tc.form)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Confirm") {
			t.Fatalf("%s: no confirmation was shown", tc.path)
		}
		for _, key := range []string{devKey, rotated, bystander} {
			if strings.Contains(body, key) {
				t.Errorf("%s: the page shows the full key %s", tc.path, prefixOf(key))
			}
		}
		d := diffOf(t, body)
		// A diff nobody can match against the row that asked for the change is
		// not a confirmation, so the prefix has to be in the diff itself.
		if !strings.Contains(d, prefixOf(devKey)) {
			t.Errorf("%s: the diff shows no key prefix:\n%s", tc.path, d)
		}
		// The grant half of the file is not a credential; blanking it would make
		// the diff unreadable for the thing it is best placed to catch.
		if !strings.Contains(d, "channels = [") || !strings.Contains(d, "ntfy") {
			t.Errorf("%s: the diff masked the channel grants:\n%s", tc.path, d)
		}
	}
}

// The same hazard on the other file: a settings diff spans the whole of
// channels.toml, so it carries every channel's credentials and not just the
// edited one's.
func TestConfirmationDiffMasksChannelCredentials(t *testing.T) {
	s, _, _ := adminServer(t)

	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/admin/channels/ntfy/settings", url.Values{"body": {
			"type = \"ntfy\"\r\nserver = \"http://127.0.0.1:1\"\r\ntopic = \"different\"\r\ntoken = \"••••••••\"\r\n"}}},
		{"/admin/channels/ntfy/delete", url.Values{}},
	} {
		_, c := etags(t, s)
		tc.form.Set("etag", c)
		rec := adminPost(t, s, tc.path, adminEmail, tc.form)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "s3cret-token") {
			t.Errorf("%s: the page shows the live token", tc.path)
		}
		d := diffOf(t, body)
		if !strings.Contains(d, "••••••••") {
			t.Errorf("%s: the token line is not masked in the diff:\n%s", tc.path, d)
		}
		// Only credentials go: an operator reviewing a channels diff is reading
		// the settings.
		if !strings.Contains(d, "127.0.0.1") {
			t.Errorf("%s: the diff masked a setting that is not a credential:\n%s", tc.path, d)
		}
	}
}

// Masking the two files before diffing them, rather than masking the rendering,
// would compare two identical masked files and tell the operator there was
// nothing to change while their new credential sat unapplied.
func TestChangingACredentialStillPreviewsAsAChange(t *testing.T) {
	s, _, chansPath := adminServer(t)
	before := read(t, chansPath)
	_, c := etags(t, s)

	rec := adminPost(t, s, "/admin/channels/ntfy/settings", adminEmail, url.Values{
		"etag": {c},
		"body": {"type = \"ntfy\"\r\nserver = \"http://127.0.0.1:1\"\r\ntopic = \"tst\"\r\ntoken = \"brand-new-token\"\r\n"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "Nothing to change") {
		t.Fatal("a changed credential was diffed as no change at all")
	}
	d := diffOf(t, body)
	if strings.Contains(d, "brand-new-token") {
		t.Errorf("the diff shows the new credential in full:\n%s", d)
	}
	if !strings.Contains(d, "− token") || !strings.Contains(d, "+ token") {
		t.Errorf("the diff does not show the token line changing:\n%s", d)
	}
	// The operator's own submission does come back, once, in the hidden field
	// that Apply reads on confirm. That is the value they typed into this page a
	// moment ago; every other appearance would be a leak.
	if n := strings.Count(body, "brand-new-token"); n != 1 {
		t.Errorf("the new credential appears %d times on the page, want once", n)
	}
	if read(t, chansPath) != before {
		t.Error("the preview step already wrote the change")
	}
}

func TestDashboardIsNotCacheable(t *testing.T) {
	s, _, _ := adminServer(t)
	rec := adminGet(t, s, "/admin", adminEmail)
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on a page listing credentials", cc)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
}
