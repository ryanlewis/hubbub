package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeysStringAndArray(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys.toml")
	write(t, p, `
[claude]
key = "nh_aaaaaaaaaaaaaaaa"
channels = ["ntfy", "email"]

# Mid-rotation: both keys valid, same caller id in the log.
[backup-host]
key = ["nh_bbbbbbbbbbbbbbbb", "nh_cccccccccccccccc"]
channels = ["ntfy"]
`)
	ring, err := LoadKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := ring.Lookup("nh_cccccccccccccccc")
	if !ok || c.ID != "backup-host" {
		t.Errorf("mid-rotation second key should resolve to backup-host, got %v %v", c, ok)
	}
	c, _ = ring.Lookup("nh_aaaaaaaaaaaaaaaa")
	if !c.Permitted("email") || c.Permitted("discord") {
		t.Error("permission boundary wrong")
	}
}

func TestLoadKeysRejectsDuplicatesAndShortKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys.toml")
	write(t, p, `
[a]
key = "nh_dddddddddddddddd"
channels = []

[b]
key = "nh_dddddddddddddddd"
channels = []
`)
	if _, err := LoadKeys(p); err == nil {
		t.Error("duplicate key must be rejected")
	}
	write(t, p, "[a]\nkey = \"short\"\nchannels = []\n")
	if _, err := LoadKeys(p); err == nil {
		t.Error("short key must be rejected")
	}
}

func TestLoadKeysRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys.toml")
	write(t, p, "[a]\nkey = \"nh_aaaaaaaaaaaaaaaa\"\nchannels = []\nchannel = [\"ntfy\"]\n")
	if _, err := LoadKeys(p); err == nil {
		t.Error("a typo'd field must fail the load rather than be silently ignored")
	}
}

func TestLoadChannelsEnabledField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	write(t, p, `
[ntfy]
type = "ntfy"
topic = "x"

[standby]
type = "ntfy"
topic = "y"
enabled = false
`)
	set, err := LoadChannels(p)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := set.Get("ntfy")
	if !ch.Enabled {
		t.Error("absent enabled must default to true")
	}
	ch, _ = set.Get("standby")
	if ch.Enabled {
		t.Error("enabled = false must disable")
	}
	if ch.Adapter != nil {
		t.Error("disabled channel must not be constructed (its config block is not validated)")
	}
	if got := len(set.Enabled()); got != 1 {
		t.Errorf("Enabled() = %d channels, want 1", got)
	}
	if got := len(set.All()); got != 2 {
		t.Errorf("All() = %d channels, want 2 (the outbox needs disabled ones to keep their spool)", got)
	}
}

// Parking a channel that has gone bad — enabled = false, stale settings
// stripped — must not take the whole file, and every healthy channel with it,
// out of service.
func TestDisabledChannelWithBrokenConfigStillLoads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	write(t, p, `
[good]
type = "ntfy"
topic = "x"

# Parked after the topic leaked; settings stripped.
[retired]
type = "ntfy"
enabled = false
`)
	set, err := LoadChannels(p)
	if err != nil {
		t.Fatalf("a disabled channel's incomplete config must not fail the load: %v", err)
	}
	if ch, ok := set.Get("good"); !ok || !ch.Enabled || ch.Adapter == nil {
		t.Error("the healthy channel must survive alongside it")
	}
}

// A parked channel's leftover settings are not validated, but they must not
// read as unknown keys either — that would be the same "one bad block takes
// the file down" failure by another route.
func TestDisabledChannelKeepsUnvalidatedSettings(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	write(t, p, `
[standby]
type = "ntfy"
enabled = false
topic = "still here"
some_setting_that_no_longer_exists = 42
`)
	if _, err := LoadChannels(p); err != nil {
		t.Errorf("a parked channel's stale settings must be tolerated: %v", err)
	}
}

// A disabled block is still typo-checked on its type name — cheap, and it
// catches the edit that would fail on re-enable.
func TestDisabledChannelRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	write(t, p, "[x]\ntype = \"carrier-pigeon\"\nenabled = false\n")
	if _, err := LoadChannels(p); err == nil {
		t.Error("unknown adapter type must fail the load even when disabled")
	}
}

// JSON kept the last of two same-named blocks and silently dropped the first,
// so a copy-pasted caller lost its key on what logged as a clean reload. TOML
// rejects duplicate keys at the parser, which is the guarantee relied on here.
func TestDuplicateIDsRejected(t *testing.T) {
	dir := t.TempDir()

	keys := filepath.Join(dir, "keys.toml")
	write(t, keys, `
[backup-host]
key = "nh_first_key_0123456789"
channels = ["ntfy"]

[backup-host]
key = "nh_second_key_012345678"
channels = ["ntfy"]
`)
	if _, err := LoadKeys(keys); err == nil {
		t.Error("a repeated caller id must fail the load, not silently drop the first block's keys")
	}

	chans := filepath.Join(dir, "channels.toml")
	write(t, chans, `
[ntfy]
type = "ntfy"
topic = "x"

[ntfy]
type = "ntfy"
topic = "y"
`)
	if _, err := LoadChannels(chans); err == nil {
		t.Error("a repeated channel id must fail the load")
	}
}

func TestLoadChannelsRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	write(t, p, "[x]\ntype = \"carrier-pigeon\"\n")
	if _, err := LoadChannels(p); err == nil {
		t.Error("unknown adapter type must fail the load")
	}
}

// An adapter's own settings are deferred-decoded, so the unknown-key check has
// to run after every block is built or a typo'd credential silently vanishes.
func TestLoadChannelsRejectsUnknownAdapterSetting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	write(t, p, "[ntfy]\ntype = \"ntfy\"\ntopic = \"x\"\ntoken_typo = \"tk_secret\"\n")
	if _, err := LoadChannels(p); err == nil {
		t.Error("a typo'd adapter setting must fail the load, not be silently dropped")
	}
}

// The channel id names the spool directory. `".."` pointed a channel's spool
// at the config directory, where the drain scan read keys.toml and
// channels.toml as long-expired items and deleted them.
func TestLoadChannelsRejectsUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.toml")
	for name, id := range map[string]string{
		"parent":    "..",
		"self":      ".",
		"separator": "ntfy/prod",
		"absolute":  "/etc/ntfy",
		"empty":     "",
		"backslash": `ntfy\prod`,
	} {
		t.Run(name, func(t *testing.T) {
			write(t, p, fmt.Sprintf("[%q]\ntype = \"ntfy\"\ntopic = \"x\"\n", id))
			if _, err := LoadChannels(p); err == nil {
				t.Errorf("channel id %q must fail the load: it is joined straight into a filesystem path", id)
			}
		})
	}
}

// A reload that fails transiently — fd exhaustion, EIO, a read that caught a
// non-atomic writer — must be retried. Stamping the mtime before the load
// landed meant one such failure swallowed that edit forever: a revoked key
// kept authenticating until someone touched the file again.
func TestReloadRetriedAfterTransientFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.toml")
	chans := filepath.Join(dir, "channels.toml")
	write(t, keys, "[a]\nkey = \"nh_aaaaaaaaaaaaaaaa\"\nchannels = [\"ntfy\"]\n")
	write(t, chans, "[ntfy]\ntype = \"ntfy\"\ntopic = \"x\"\n")

	store, err := NewStore(&Config{KeysFile: keys, ChannelsFile: chans})
	if err != nil {
		t.Fatal(err)
	}

	// Revoke the leaked key, then make the read fail.
	write(t, keys, "[b]\nkey = \"nh_bbbbbbbbbbbbbbbb\"\nchannels = [\"ntfy\"]\n")
	if err := os.Chmod(keys, 0o000); err != nil {
		t.Fatal(err)
	}
	store.pollOnce()
	if _, ok := store.Keyring().Lookup("nh_aaaaaaaaaaaaaaaa"); !ok {
		t.Fatal("an unreadable keys file must keep the previous keyring")
	}

	if err := os.Chmod(keys, 0o600); err != nil {
		t.Fatal(err)
	}
	store.pollOnce()
	if _, ok := store.Keyring().Lookup("nh_bbbbbbbbbbbbbbbb"); !ok {
		t.Error("the reload was never retried: the edit is permanently swallowed")
	}
	if _, ok := store.Keyring().Lookup("nh_aaaaaaaaaaaaaaaa"); ok {
		t.Error("the revoked key still authenticates")
	}
}

// Two writes can share one filesystem timestamp — the dashboard saving twice in
// a second — and an editor can put an mtime back deliberately. An mtime-gated
// poll misses those *permanently*, because it compares against the last loaded
// state: no later poll sees a difference either, so the second revocation never
// takes effect.
func TestStoreReloadsWhenTheTimestampDoesNotMove(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.toml")
	chans := filepath.Join(dir, "channels.toml")
	write(t, keys, "[a]\nkey = \"nh_aaaaaaaaaaaaaaaa\"\nchannels = [\"ntfy\"]\n")
	write(t, chans, "[ntfy]\ntype = \"ntfy\"\ntopic = \"x\"\n")

	store, err := NewStore(&Config{KeysFile: keys, ChannelsFile: chans})
	if err != nil {
		t.Fatal(err)
	}
	stamp := mtimeOf(t, keys)

	write(t, keys, "[a]\nkey = \"nh_ccccccccccccccccc\"\nchannels = [\"ntfy\"]\n")
	if err := os.Chtimes(keys, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := mtimeOf(t, keys); !got.Equal(stamp) {
		t.Fatalf("could not hold the mtime still (%v vs %v)", got, stamp)
	}

	store.pollOnce()
	if _, ok := store.Keyring().Lookup("nh_ccccccccccccccccc"); !ok {
		t.Error("the new key never loaded: a change with an unmoved mtime was missed")
	}
	if _, ok := store.Keyring().Lookup("nh_aaaaaaaaaaaaaaaa"); ok {
		t.Error("the revoked key still authenticates")
	}
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestStoreValidateBeforeSwap(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.toml")
	chans := filepath.Join(dir, "channels.toml")
	write(t, keys, "[a]\nkey = \"nh_aaaaaaaaaaaaaaaa\"\nchannels = [\"ntfy\"]\n")
	write(t, chans, "[ntfy]\ntype = \"ntfy\"\ntopic = \"x\"\n")

	store, err := NewStore(&Config{KeysFile: keys, ChannelsFile: chans})
	if err != nil {
		t.Fatal(err)
	}

	// Break the keys file: a half-written hand-edit must NOT blank creds.
	write(t, keys, "[a]\nkey = \"nh_aaaaaaaa") // truncated mid-string
	store.pollOnce()
	if _, ok := store.Keyring().Lookup("nh_aaaaaaaaaaaaaaaa"); !ok {
		t.Error("broken keys file must keep the previous keyring")
	}

	// Fix it with a new caller: swap should now happen.
	write(t, keys, "[b]\nkey = \"nh_bbbbbbbbbbbbbbbb\"\nchannels = []\n")
	store.pollOnce()
	if _, ok := store.Keyring().Lookup("nh_bbbbbbbbbbbbbbbb"); !ok {
		t.Error("valid keys file must swap in")
	}
	if _, ok := store.Keyring().Lookup("nh_aaaaaaaaaaaaaaaa"); ok {
		t.Error("old key should be gone after successful reload")
	}
}

// The defaults only apply to absent fields, so an explicit 0 reaches the
// runtime as-is — and a zero rate cap, attempt timeout or spool cap each break
// the hub in a way nothing alerts on.
func TestLoadConfigRejectsNonPositiveKnobs(t *testing.T) {
	const base = "public_port = 8080\n"
	cases := map[string]string{
		"rate_cap_per_hour":     `rate_cap_per_hour = 0`,
		"spool_cap_per_channel": `spool_cap_per_channel = 0`,
		"attempt_timeout":       `attempt_timeout = "0s"`,
		"response_window":       `response_window = "0s"`,
		"queue_ttl":             `queue_ttl = "0s"`,
		"drain_pace":            `drain_pace = "0s"`,
	}
	for name, field := range cases {
		dir := t.TempDir()
		p := filepath.Join(dir, "hubbub.toml")
		write(t, p, base+field+"\n")
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s = 0 must be rejected at load", name)
		}
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "hubbub.toml")
	write(t, p, base)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("a minimal config must still load: %v", err)
	}
	if cfg.RateCapPerHour != 60 {
		t.Errorf("rate_cap_per_hour = %d, want the 60 default when absent", cfg.RateCapPerHour)
	}
}

// The example files are both the documented starting point and what
// `go run . -config example/hubbub.toml` actually loads, so a broken one is a
// broken first run. TOML makes one mistake easy to miss: every top-level
// setting must come before the first [table] header, or it silently becomes a
// key of that table instead — an [ops] block sitting mid-file absorbed every
// setting after it, and the shipped example refused to start.
func TestShippedExampleConfigLoads(t *testing.T) {
	const root = "../.."
	cfg, err := LoadConfig(filepath.Join(root, "example", "hubbub.toml"))
	if err != nil {
		t.Fatalf("the shipped example config must load: %v", err)
	}

	// Assert on settings whose example values differ from their defaults. An
	// absorbed setting falls back to its default, so checking one that happens
	// to match the default (public_port, rate_cap_per_hour) would pass straight
	// through the bug this guards.
	if cfg.SpoolDir != "example/spool" {
		t.Errorf("spool_dir = %q, want \"example/spool\" — a top-level setting has been absorbed into a [table]", cfg.SpoolDir)
	}
	if cfg.DeliveryLog != "example/delivery.log" {
		t.Errorf("delivery_log = %q, want \"example/delivery.log\" — a top-level setting has been absorbed into a [table]", cfg.DeliveryLog)
	}
	if cfg.Ops == nil || cfg.Ops.Port != 2112 {
		t.Errorf("ops = %+v, want the example's port 2112", cfg.Ops)
	}

	// The paths inside the example config are written relative to the repo
	// root, because that's where the documented `go run .` is invoked.
	ring, err := LoadKeys(filepath.Join(root, cfg.KeysFile))
	if err != nil {
		t.Fatalf("keys_file %q must load from the repo root: %v", cfg.KeysFile, err)
	}
	set, err := LoadChannels(filepath.Join(root, cfg.ChannelsFile))
	if err != nil {
		t.Fatalf("channels_file %q must load from the repo root: %v", cfg.ChannelsFile, err)
	}

	// A permission naming a channel with no config block reports `disabled`,
	// which is indistinguishable from a deliberate toggle — not a first run
	// worth handing anyone. Keep the shipped pair consistent.
	for _, c := range ring.byKey {
		for _, id := range c.Channels {
			if _, ok := set.Get(id); !ok {
				t.Errorf("example keys grant caller %q the channel %q, which example/channels.toml does not configure", c.ID, id)
			}
		}
	}
}

func TestLoadConfigRejectsUnknownSetting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hubbub.toml")
	write(t, p, "public_port = 8080\nrate_cap_per_hor = 60\n")
	if _, err := LoadConfig(p); err == nil {
		t.Error("a typo'd setting must fail the load rather than silently take its default")
	}
}

// Comments are the whole reason for the format change; make sure a realistic
// commented file actually parses, including the presence-enabled tables.
func TestLoadConfigWithCommentsAndTables(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hubbub.toml")
	write(t, p, `
# hubbub config
public_port = 8080      # caller-facing API
response_window = "2.5s"

[ops]
port = 2112             # keep off the public internet

[heartbeat]
url = "https://hc-ping.com/uuid"
# interval omitted on purpose: it should default
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ops == nil || cfg.Ops.Port != 2112 {
		t.Errorf("ops = %+v, want port 2112", cfg.Ops)
	}
	if cfg.Heartbeat == nil || cfg.Heartbeat.Interval.Duration <= 0 {
		t.Errorf("heartbeat = %+v, want a defaulted interval", cfg.Heartbeat)
	}
	if cfg.ResponseWindow.Duration.String() != "2.5s" {
		t.Errorf("response_window = %v, want 2.5s", cfg.ResponseWindow)
	}
}
