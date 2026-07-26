package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The admin dashboard validates a candidate edit before writing it, so both
// loaders have to work from bytes. Going through a temp file instead would
// leave a 0600 file full of live credentials in the config directory whenever
// the process died mid-validation.
func TestParseFromBytesMatchesLoadFromFile(t *testing.T) {
	dir := t.TempDir()

	keys := `
[ops]
key = "0123456789abcdef0123"
channels = ["ntfy"]
`
	kp := filepath.Join(dir, "keys.toml")
	write(t, kp, keys)
	fromFile, err := LoadKeys(kp)
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}
	fromBytes, err := ParseKeys([]byte(keys), "candidate")
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	a, okA := fromFile.Lookup("0123456789abcdef0123")
	b, okB := fromBytes.Lookup("0123456789abcdef0123")
	if !okA || !okB || a.ID != b.ID {
		t.Errorf("file and bytes disagree: %v/%v %+v %+v", okA, okB, a, b)
	}

	chans := `
[ntfy]
type = "ntfy"
topic = "t"
`
	cp := filepath.Join(dir, "channels.toml")
	write(t, cp, chans)
	cFile, err := LoadChannels(cp)
	if err != nil {
		t.Fatalf("LoadChannels: %v", err)
	}
	cBytes, err := ParseChannels([]byte(chans), "candidate")
	if err != nil {
		t.Fatalf("ParseChannels: %v", err)
	}
	if len(cFile.IDs()) != 1 || len(cBytes.IDs()) != 1 || cFile.IDs()[0] != cBytes.IDs()[0] {
		t.Errorf("file %v vs bytes %v", cFile.IDs(), cBytes.IDs())
	}
}

// The error has to name what the operator is looking at, not a scratch path
// they have never seen.
func TestParseErrorsUseTheGivenName(t *testing.T) {
	_, err := ParseKeys([]byte("[ops]\nkey = \"0123456789abcdef0123\"\n"), "your edit")
	if err == nil || !strings.Contains(err.Error(), "your edit") {
		t.Errorf("err = %v, want it to mention %q", err, "your edit")
	}
	_, err = ParseChannels([]byte("[x]\n"), "your edit")
	if err == nil || !strings.Contains(err.Error(), "your edit") {
		t.Errorf("err = %v, want it to mention %q", err, "your edit")
	}
}

func TestValidCallerID(t *testing.T) {
	ok := []string{"dev", "claude-routines", "ops_bot", "a", "A1"}
	for _, id := range ok {
		if err := validCallerID(id); err != nil {
			t.Errorf("validCallerID(%q) = %v, want nil", id, err)
		}
	}
	// A dotted id is legal TOML when quoted, but ["ops.team"] and [ops.team]
	// then look alike while meaning entirely different things — and the
	// dashboard locates a caller by matching its header in the source text.
	bad := []string{"", "ops.team", "a/b", "a b", "a[b", "a\"b", strings.Repeat("x", 65)}
	for _, id := range bad {
		if err := validCallerID(id); err == nil {
			t.Errorf("validCallerID(%q) = nil, want an error", id)
		}
	}
}

func TestParseKeysRejectsBadCallerID(t *testing.T) {
	_, err := ParseKeys([]byte("[\"ops.team\"]\nkey = \"0123456789abcdef0123\"\nchannels = []\n"), "k")
	if err == nil || !strings.Contains(err.Error(), "caller id") {
		t.Errorf("err = %v, want a caller-id complaint", err)
	}
}

// The ring is indexed by key, so a caller mid-rotation is reachable under two
// of them and must still appear exactly once.
func TestKeyringCallersDeduplicatesRotatingCallers(t *testing.T) {
	ring, err := ParseKeys([]byte(`
[zeta]
key = "zzzzzzzzzzzzzzzzzzzz"
channels = ["ntfy"]

[alpha]
key = ["aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"]
channels = ["ntfy", "email"]
`), "k")
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	got := ring.Callers()
	if len(got) != 2 {
		t.Fatalf("Callers() returned %d entries, want 2: %+v", len(got), got)
	}
	if got[0].ID != "alpha" || got[1].ID != "zeta" {
		t.Errorf("Callers() = [%s %s], want sorted [alpha zeta]", got[0].ID, got[1].ID)
	}
	if len(got[0].Keys) != 2 {
		t.Errorf("alpha has %d keys, want both halves of the rotation", len(got[0].Keys))
	}
}

// ReloadNow must not reload inline: every mtime field on Store is
// unsynchronised and safe only because the watcher alone touches them. This
// test is the -race regression guard for that.
func TestReloadNowIsRaceFreeAlongsideWatch(t *testing.T) {
	dir := t.TempDir()
	kp := filepath.Join(dir, "keys.toml")
	cp := filepath.Join(dir, "channels.toml")
	write(t, kp, "[a]\nkey = \"0123456789abcdef0123\"\nchannels = []\n")
	write(t, cp, "[ntfy]\ntype = \"ntfy\"\ntopic = \"t\"\n")

	store, err := NewStore(&Config{KeysFile: kp, ChannelsFile: cp})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.Watch(ctx, time.Millisecond)

	for i := range 50 {
		write(t, kp, "[a]\nkey = \"0123456789abcdef012"+string(rune('a'+i%26))+"\"\nchannels = []\n")
		store.ReloadNow()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.Keyring().Lookup("0123456789abcdef012" + string(rune('a'+49%26))); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the final edit never became live")
}

// A signal with no watcher running must not block the caller — the dashboard
// would otherwise deadlock on a hub whose watcher had stopped.
func TestReloadNowDoesNotBlockWithoutAWatcher(t *testing.T) {
	dir := t.TempDir()
	kp := filepath.Join(dir, "keys.toml")
	cp := filepath.Join(dir, "channels.toml")
	write(t, kp, "[a]\nkey = \"0123456789abcdef0123\"\nchannels = []\n")
	write(t, cp, "[ntfy]\ntype = \"ntfy\"\ntopic = \"t\"\n")
	store, err := NewStore(&Config{KeysFile: kp, ChannelsFile: cp})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for range 10 {
			store.ReloadNow()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ReloadNow blocked with no watcher draining the channel")
	}
}

func TestAdminConfig(t *testing.T) {
	base := `
public_port = 8080
spool_dir = "s"
`
	load := func(t *testing.T, extra string) (*Config, error) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "hubbub.toml")
		write(t, p, base+extra)
		return LoadConfig(p)
	}

	t.Run("absent means off", func(t *testing.T) {
		cfg, err := load(t, "")
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Admin != nil {
			t.Errorf("Admin = %+v, want nil when [admin] is absent", cfg.Admin)
		}
	})

	t.Run("valid block loads", func(t *testing.T) {
		cfg, err := load(t, "\n[admin]\nauth = \"exe-dev\"\nallowed_emails = [\"a@b.c\"]\n")
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Admin == nil || cfg.Admin.Auth != "exe-dev" || len(cfg.Admin.AllowedEmails) != 1 {
			t.Errorf("Admin = %+v", cfg.Admin)
		}
	})

	t.Run("empty allowlist refuses to start", func(t *testing.T) {
		_, err := load(t, "\n[admin]\nauth = \"exe-dev\"\nallowed_emails = []\n")
		if err == nil || !strings.Contains(err.Error(), "allowed_emails") {
			t.Errorf("err = %v, want a complaint about allowed_emails", err)
		}
	})

	t.Run("missing allowlist refuses to start", func(t *testing.T) {
		_, err := load(t, "\n[admin]\nauth = \"exe-dev\"\n")
		if err == nil || !strings.Contains(err.Error(), "allowed_emails") {
			t.Errorf("err = %v, want a complaint about allowed_emails", err)
		}
	})

	t.Run("unknown provider fails at load", func(t *testing.T) {
		_, err := load(t, "\n[admin]\nauth = \"google-sso\"\nallowed_emails = [\"a@b.c\"]\n")
		if err == nil || !strings.Contains(err.Error(), "exe-dev") {
			t.Errorf("err = %v, want the known providers listed", err)
		}
	})

	t.Run("missing auth fails at load", func(t *testing.T) {
		_, err := load(t, "\n[admin]\nallowed_emails = [\"a@b.c\"]\n")
		if err == nil || !strings.Contains(err.Error(), "admin.auth") {
			t.Errorf("err = %v, want a complaint about admin.auth", err)
		}
	})

	t.Run("typo'd setting fails at load", func(t *testing.T) {
		_, err := load(t, "\n[admin]\nauth = \"exe-dev\"\nallowed_email = [\"a@b.c\"]\n")
		if err == nil || !strings.Contains(err.Error(), "unknown setting") {
			t.Errorf("err = %v, want an unknown-setting error", err)
		}
	})
}

func TestLoadKeysReportsMissingFile(t *testing.T) {
	if _, err := LoadKeys(filepath.Join(t.TempDir(), "nope.toml")); !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}
