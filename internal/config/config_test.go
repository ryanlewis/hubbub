package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeysStringAndArray(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys.json")
	write(t, p, `{
		"claude": { "key": "nh_aaaaaaaaaaaaaaaa", "channels": ["ntfy","email"] },
		"betty":  { "key": ["nh_bbbbbbbbbbbbbbbb","nh_cccccccccccccccc"], "channels": ["ntfy"] }
	}`)
	ring, err := LoadKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := ring.Lookup("nh_cccccccccccccccc")
	if !ok || c.ID != "betty" {
		t.Errorf("mid-rotation second key should resolve to betty, got %v %v", c, ok)
	}
	c, _ = ring.Lookup("nh_aaaaaaaaaaaaaaaa")
	if !c.Permitted("email") || c.Permitted("discord") {
		t.Error("permission boundary wrong")
	}
}

func TestLoadKeysRejectsDuplicatesAndShortKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys.json")
	write(t, p, `{
		"a": { "key": "nh_dddddddddddddddd", "channels": [] },
		"b": { "key": "nh_dddddddddddddddd", "channels": [] }
	}`)
	if _, err := LoadKeys(p); err == nil {
		t.Error("duplicate key must be rejected")
	}
	write(t, p, `{"a": { "key": "short", "channels": [] }}`)
	if _, err := LoadKeys(p); err == nil {
		t.Error("short key must be rejected")
	}
}

func TestLoadChannelsEnabledField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.json")
	write(t, p, `{
		"ntfy":    { "type": "ntfy", "topic": "x" },
		"standby": { "type": "ntfy", "topic": "y", "enabled": false }
	}`)
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
		t.Error("enabled:false must disable")
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

// Parking a channel that has gone bad — enabled:false, stale settings
// stripped — must not take the whole file, and every healthy channel with it,
// out of service.
func TestDisabledChannelWithBrokenConfigStillLoads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.json")
	write(t, p, `{
		"good":    { "type": "ntfy", "topic": "x" },
		"retired": { "type": "ntfy", "enabled": false }
	}`)
	set, err := LoadChannels(p)
	if err != nil {
		t.Fatalf("a disabled channel's incomplete config must not fail the load: %v", err)
	}
	if ch, ok := set.Get("good"); !ok || !ch.Enabled || ch.Adapter == nil {
		t.Error("the healthy channel must survive alongside it")
	}
}

// A disabled block is still typo-checked on its type name — cheap, and it
// catches the edit that would fail on re-enable.
func TestDisabledChannelRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.json")
	write(t, p, `{"x": { "type": "carrier-pigeon", "enabled": false }}`)
	if _, err := LoadChannels(p); err == nil {
		t.Error("unknown adapter type must fail the load even when disabled")
	}
}

func TestDuplicateIDsRejected(t *testing.T) {
	dir := t.TempDir()

	keys := filepath.Join(dir, "keys.json")
	write(t, keys, `{
		"betty": { "key": "nh_first_key_0123456789", "channels": ["ntfy"] },
		"betty": { "key": "nh_second_key_012345678", "channels": ["ntfy"] }
	}`)
	if _, err := LoadKeys(keys); err == nil {
		t.Error("a repeated caller id must fail the load, not silently drop the first block's keys")
	}

	chans := filepath.Join(dir, "channels.json")
	write(t, chans, `{
		"ntfy": { "type": "ntfy", "topic": "x" },
		"ntfy": { "type": "ntfy", "topic": "y" }
	}`)
	if _, err := LoadChannels(chans); err == nil {
		t.Error("a repeated channel id must fail the load")
	}
}

func TestLoadChannelsRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.json")
	write(t, p, `{"x": { "type": "carrier-pigeon" }}`)
	if _, err := LoadChannels(p); err == nil {
		t.Error("unknown adapter type must fail the load")
	}
}

// The channel id names the spool directory. `".."` pointed a channel's spool
// at the config directory, where the drain scan read keys.json and
// channels.json as long-expired items and deleted them.
func TestLoadChannelsRejectsUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "channels.json")
	for name, id := range map[string]string{
		"parent":    "..",
		"self":      ".",
		"separator": "ntfy/prod",
		"absolute":  "/etc/ntfy",
		"empty":     "",
		"backslash": `ntfy\prod`,
	} {
		t.Run(name, func(t *testing.T) {
			write(t, p, fmt.Sprintf(`{%q: { "type": "ntfy", "topic": "x" }}`, id))
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
	keys := filepath.Join(dir, "keys.json")
	chans := filepath.Join(dir, "channels.json")
	write(t, keys, `{"a": { "key": "nh_aaaaaaaaaaaaaaaa", "channels": ["ntfy"] }}`)
	write(t, chans, `{"ntfy": { "type": "ntfy", "topic": "x" }}`)

	store, err := NewStore(&Config{KeysFile: keys, ChannelsFile: chans})
	if err != nil {
		t.Fatal(err)
	}

	// Revoke the leaked key, then make the read fail. chmod moves ctime, not
	// mtime, so the poll still sees exactly one content change.
	write(t, keys, `{"b": { "key": "nh_bbbbbbbbbbbbbbbb", "channels": ["ntfy"] }}`)
	store.keysMtime = mtime(keys).Add(-1) // force change detection
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

func TestStoreValidateBeforeSwap(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys.json")
	chans := filepath.Join(dir, "channels.json")
	write(t, keys, `{"a": { "key": "nh_aaaaaaaaaaaaaaaa", "channels": ["ntfy"] }}`)
	write(t, chans, `{"ntfy": { "type": "ntfy", "topic": "x" }}`)

	store, err := NewStore(&Config{KeysFile: keys, ChannelsFile: chans})
	if err != nil {
		t.Fatal(err)
	}

	// Break the keys file: a half-written hand-edit must NOT blank creds.
	write(t, keys, `{"a": { "key": "nh_aaaaaaaa`) // truncated JSON
	store.keysMtime = mtime(keys).Add(-1)         // force change detection
	store.pollOnce()
	if _, ok := store.Keyring().Lookup("nh_aaaaaaaaaaaaaaaa"); !ok {
		t.Error("broken keys file must keep the previous keyring")
	}

	// Fix it with a new caller: swap should now happen.
	write(t, keys, `{"b": { "key": "nh_bbbbbbbbbbbbbbbb", "channels": [] }}`)
	store.keysMtime = mtime(keys).Add(-1)
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
	base := `"publicPort": 8080`
	cases := map[string]string{
		"rateCapPerHour":     `"rateCapPerHour": 0`,
		"spoolCapPerChannel": `"spoolCapPerChannel": 0`,
		"attemptTimeout":     `"attemptTimeout": "0s"`,
		"responseWindow":     `"responseWindow": "0s"`,
		"queueTTL":           `"queueTTL": "0s"`,
		"drainPace":          `"drainPace": "0s"`,
	}
	for name, field := range cases {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.json")
		write(t, p, "{"+base+", "+field+"}")
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s = 0 must be rejected at load", name)
		}
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	write(t, p, "{"+base+"}")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("a minimal config must still load: %v", err)
	}
	if cfg.RateCapPerHour != 60 {
		t.Errorf("rateCapPerHour = %d, want the 60 default when absent", cfg.RateCapPerHour)
	}
}
