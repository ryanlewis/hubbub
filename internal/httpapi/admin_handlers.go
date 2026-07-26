package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ryanlewis/hubbub/internal/adapter"
	"github.com/ryanlewis/hubbub/internal/confedit"
	"github.com/ryanlewis/hubbub/internal/config"
	"github.com/ryanlewis/hubbub/internal/dlog"
	"github.com/ryanlewis/hubbub/internal/notify"
	"github.com/ryanlewis/hubbub/internal/tomledit"
)

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, &adminView{Editing: r.URL.Query().Get("edit")}, http.StatusOK)
}

// verifiedSpan locates a table and checks the answer against the real parser
// before anything is spliced. The locator is a scanner with pragmatic
// shortcuts; this is the independent oracle that makes trusting it safe.
func verifiedSpan(src []byte, id string) error {
	start, end, err := tomledit.Span(src, id)
	if err != nil {
		return err
	}
	return tomledit.Verify(src, start, end, id)
}

// preview renders a diff for confirmation and reports whether it handled the
// request.
//
// Nothing structural can detect comment loss — a comment has no semantics — so
// for freeform and destructive edits the last line of defence is showing the
// operator what is about to change. "-40 +2" on a settings tweak is the shape
// that makes an accidental documentation deletion obvious.
func (s *Server) preview(w http.ResponseWriter, r *http.Request, f *confedit.File, etag string, edit confedit.Edit, cv confirmView) bool {
	if r.FormValue("confirm") == "1" {
		return false
	}
	src, cur, err := f.Read()
	if err != nil {
		s.fail(w, r, err)
		return true
	}
	if etag != "" && etag != cur {
		s.fail(w, r, confedit.ErrConflict)
		return true
	}
	out, err := edit(src)
	if err != nil {
		s.fail(w, r, err)
		return true
	}
	d := confedit.Diff(src, out)
	if !confedit.Changed(d) {
		s.ok(w, r, "Nothing to change.")
		return true
	}
	cv.Diff = maskDiff(d, s.maskFor(f))
	cv.Added, cv.Deleted = confedit.Counts(d)
	if cv.Fields == nil {
		cv.Fields = map[string]string{}
	}
	cv.Fields["etag"] = etag
	s.renderAdmin(w, r, &adminView{Confirm: &cv}, http.StatusOK)
	return true
}

// --- channels ---------------------------------------------------------------

func (s *Server) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	typ := strings.TrimSpace(r.FormValue("type"))

	if err := config.ValidChannelID(id); err != nil {
		s.fail(w, r, err)
		return
	}
	if !adapter.Known(typ) {
		s.fail(w, r, fmt.Errorf("unknown adapter type %q (known: %s)", typ, strings.Join(adapter.Types(), ", ")))
		return
	}

	etag := r.FormValue("etag")
	_, err := s.Admin.Channels.Apply(etag, func(src []byte) ([]byte, error) {
		if slices.Contains(tomledit.IDs(src), id) {
			return nil, fmt.Errorf("a channel called %q already exists", id)
		}
		// Created disabled on purpose. An adapter validates its settings at
		// construction, so a channel with nothing but a type could never
		// load — while a disabled one is type-checked and parked, which is
		// exactly the state a half-configured channel should be in.
		block := fmt.Sprintf("[%s]\ntype = %s\nenabled = false\n", id, tomlString(typ))
		return tomledit.AppendBlock(src, []byte(block)), nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "channel_create", id)
	s.ok(w, r, fmt.Sprintf("Created %q, disabled. Fill in its settings, then enable it.", id))
}

func (s *Server) handleChannelToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	enable := r.FormValue("enable") == "1"
	etag := r.FormValue("etag")

	_, err := s.Admin.Channels.Apply(etag, func(src []byte) ([]byte, error) {
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		out, err := tomledit.SetKeyInBlock(src, id, "enabled", fmt.Sprintf("%t", enable))
		if err != nil {
			return nil, err
		}
		return out, tomledit.VerifyNeighbours(src, out, id)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	action, word := "channel_disable", "Disabled"
	if enable {
		action, word = "channel_enable", "Enabled"
	}
	s.audit(r, action, id)
	// Disabling is a pause, not a purge: the spool survives so anything queued
	// goes out when the channel comes back.
	note := fmt.Sprintf("%s %q.", word, id)
	if !enable {
		note += " Its spool is kept, so queued messages will go out if you re-enable it."
	}
	s.ok(w, r, note)
}

func (s *Server) handleChannelSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	etag := r.FormValue("etag")
	submitted := r.FormValue("body")

	edit := func(src []byte) ([]byte, error) {
		// A textarea always submits CRLF, whatever the file uses.
		body := []byte(tomledit.Normalise(submitted, src))
		// The header belongs to the form, not the textarea. Typing over
		// [ntfy] would read to the outbox as "channel removed from config",
		// which settles the backlog and deletes the spool directory.
		if tomledit.HasTableHeader(body) {
			return nil, errors.New("settings must not contain a [table] header — the channel's name is not editable here, and a sub-table has to be added over SSH")
		}
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		original, err := tomledit.Body(src, id)
		if err != nil {
			return nil, err
		}
		body, err = unmaskSecrets(body, original)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(body)) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
			body = append(body, '\n')
		}
		out, err := tomledit.ReplaceBody(src, id, body)
		if err != nil {
			return nil, err
		}
		if err := verifiedSpan(out, id); err != nil {
			return nil, err
		}
		return out, tomledit.VerifyNeighbours(src, out, id)
	}

	if s.preview(w, r, s.Admin.Channels, etag, edit, confirmView{
		Title:  fmt.Sprintf("Save settings for %q?", id),
		Action: "/admin/channels/" + id + "/settings",
		Fields: map[string]string{"body": submitted},
	}) {
		return
	}
	if _, err := s.Admin.Channels.Apply(etag, edit); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "channel_settings", id)
	s.ok(w, r, fmt.Sprintf("Saved settings for %q.", id))
}

func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	etag := r.FormValue("etag")

	edit := func(src []byte) ([]byte, error) {
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		return tomledit.RemoveBlock(src, id)
	}

	// Removing a channel from config is materially different from disabling
	// it: the engine settles the backlog and removes the spool directory.
	// Name that, and name anyone still permitted to send to it.
	title := fmt.Sprintf("Delete channel %q?", id)
	warn := " Its spool will be settled and removed — anything still queued for it is gone."
	if holders := s.callersGranting(id); len(holders) > 0 {
		warn += fmt.Sprintf(" Still granted to: %s.", strings.Join(holders, ", "))
	}
	if s.preview(w, r, s.Admin.Channels, etag, edit, confirmView{
		Title:  title + warn,
		Action: "/admin/channels/" + id + "/delete",
	}) {
		return
	}
	if _, err := s.Admin.Channels.Apply(etag, edit); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "channel_delete", id)
	s.ok(w, r, fmt.Sprintf("Deleted %q.", id))
}

// callersGranting lists callers still permitted to send to a channel.
func (s *Server) callersGranting(id string) []string {
	var out []string
	for _, c := range s.Store.Keyring().Callers() {
		if c.Permitted(id) {
			out = append(out, c.ID)
		}
	}
	return out
}

// handleChannelTest fires the same canned send as the ops-port CTA, so a
// channel can be proven from the page that just configured it.
func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, exists := s.Store.Channels().Get(id)
	if !exists {
		s.fail(w, r, fmt.Errorf("unknown channel %q", id))
		return
	}
	if !ch.Enabled {
		s.fail(w, r, fmt.Errorf("channel %q is disabled; enable it before testing", id))
		return
	}

	n := notify.Notification{
		Title:     "hubbub test",
		Message:   fmt.Sprintf("test send to channel %q at %s", id, time.Now().UTC().Format(time.RFC3339)),
		Priority:  notify.PriorityDefault,
		CallerID:  "admin-test",
		RequestID: notify.NewRequestID(),
		CreatedAt: time.Now().UTC(),
	}
	results := s.dispatch(n, []string{id}, true)
	_, result := statusFor(results)
	s.Log.Append(dlog.Record{
		Kind:      "request",
		RequestID: n.RequestID,
		CallerID:  n.CallerID,
		Result:    result,
		Channels:  results,
		Title:     n.Title,
	})
	s.ok(w, r, fmt.Sprintf("Test send to %q: %s (%s).", id, result, results[id]))
}

// --- callers ----------------------------------------------------------------

func (s *Server) handleCallerCreate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	etag := r.FormValue("etag")

	key := newKey()
	_, err := s.Admin.Keys.Apply(etag, func(src []byte) ([]byte, error) {
		if slices.Contains(tomledit.IDs(src), id) {
			return nil, fmt.Errorf("a caller called %q already exists", id)
		}
		// Deny by default: a new caller reaches nothing until it is granted
		// something. An explicit empty list is legal and is not the same as
		// omitting the field, which fails the load.
		block := fmt.Sprintf("[%s]\nkey = %s\nchannels = []\n", id, tomlString(key))
		return tomledit.AppendBlock(src, []byte(block)), nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "caller_create", id)
	s.renderAdmin(w, r, &adminView{
		Notice: fmt.Sprintf("Created caller %q with no channel grants yet.", id),
		NewKey: &newKeyView{CallerID: id, Key: key},
	}, http.StatusOK)
}

func (s *Server) handleCallerGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	etag := r.FormValue("etag")
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err)
		return
	}
	granted := r.Form["channel"]

	// A grant naming a channel that does not exist is dead config that looks
	// live; the delivery result would be a silent "disabled".
	known := s.Store.Channels().IDs()
	for _, g := range granted {
		if !slices.Contains(known, g) {
			s.fail(w, r, fmt.Errorf("no channel called %q", g))
			return
		}
	}
	slices.Sort(granted)

	_, err := s.Admin.Keys.Apply(etag, func(src []byte) ([]byte, error) {
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		out, err := tomledit.SetKeyInBlock(src, id, "channels", tomlStringArray(granted))
		if err != nil {
			return nil, err
		}
		return out, tomledit.VerifyNeighbours(src, out, id)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "caller_grants", id)
	list := "nothing"
	if len(granted) > 0 {
		list = strings.Join(granted, ", ")
	}
	s.ok(w, r, fmt.Sprintf("%q may now send to %s.", id, list))
}

// handleCallerRotate adds a second key without removing the first, which is
// what makes rotation a no-outage operation: add, flip the caller over, then
// revoke the old one.
func (s *Server) handleCallerRotate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	etag := r.FormValue("etag")

	key := newKey()
	_, err := s.Admin.Keys.Apply(etag, func(src []byte) ([]byte, error) {
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		ring, err := config.ParseKeys(src, s.Admin.Keys.Path)
		if err != nil {
			return nil, err
		}
		c, ok := findCaller(ring, id)
		if !ok {
			return nil, fmt.Errorf("no caller called %q", id)
		}
		out, err := tomledit.SetKeyInBlock(src, id, "key", tomlStringArray(append(slices.Clone(c.Keys), key)))
		if err != nil {
			return nil, err
		}
		return out, tomledit.VerifyNeighbours(src, out, id)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "caller_rotate", id)
	s.renderAdmin(w, r, &adminView{
		Notice: fmt.Sprintf("Added a second key for %q. Both work now — move the caller over, then revoke the old one.", id),
		NewKey: &newKeyView{CallerID: id, Key: key},
	}, http.StatusOK)
}

func (s *Server) handleCallerRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prefix := r.FormValue("prefix")
	etag := r.FormValue("etag")

	edit := func(src []byte) ([]byte, error) {
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		ring, err := config.ParseKeys(src, s.Admin.Keys.Path)
		if err != nil {
			return nil, err
		}
		c, ok := findCaller(ring, id)
		if !ok {
			return nil, fmt.Errorf("no caller called %q", id)
		}
		remaining := make([]string, 0, len(c.Keys))
		found := false
		for _, k := range c.Keys {
			if prefixOf(k) == prefix {
				found = true
				continue
			}
			remaining = append(remaining, k)
		}
		if !found {
			return nil, fmt.Errorf("%q has no key starting %s", id, prefix)
		}
		// A caller with no key at all fails the load, which would take every
		// other caller down with it. Deleting the caller is the operation
		// they actually want.
		if len(remaining) == 0 {
			return nil, fmt.Errorf("that is the only key %q has; delete the caller instead, or rotate first", id)
		}
		out, err := tomledit.SetKeyInBlock(src, id, "key", tomlStringArray(remaining))
		if err != nil {
			return nil, err
		}
		return out, tomledit.VerifyNeighbours(src, out, id)
	}

	if s.preview(w, r, s.Admin.Keys, etag, edit, confirmView{
		Title:  fmt.Sprintf("Revoke key %s from %q? Anything still using it starts getting 401s within seconds.", prefix, id),
		Action: "/admin/callers/" + id + "/revoke",
		Fields: map[string]string{"prefix": prefix},
	}) {
		return
	}
	if _, err := s.Admin.Keys.Apply(etag, edit); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "caller_revoke", id)
	s.ok(w, r, fmt.Sprintf("Revoked key %s from %q.", prefix, id))
}

func (s *Server) handleCallerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	etag := r.FormValue("etag")

	edit := func(src []byte) ([]byte, error) {
		if err := verifiedSpan(src, id); err != nil {
			return nil, err
		}
		return tomledit.RemoveBlock(src, id)
	}

	if s.preview(w, r, s.Admin.Keys, etag, edit, confirmView{
		Title:  fmt.Sprintf("Delete caller %q? Every key it holds stops working immediately.", id),
		Action: "/admin/callers/" + id + "/delete",
	}) {
		return
	}
	if _, err := s.Admin.Keys.Apply(etag, edit); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "caller_delete", id)
	s.ok(w, r, fmt.Sprintf("Deleted caller %q.", id))
}

func findCaller(ring *config.Keyring, id string) (*config.Caller, bool) {
	for _, c := range ring.Callers() {
		if c.ID == id {
			return c, true
		}
	}
	return nil, false
}
