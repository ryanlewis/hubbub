package config

import (
	"fmt"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/ryanlewis/hubbub/internal/adapter"
)

// Channel is a named channel instance: the entry key is the channel id
// (what permissions, per-channel results and the OpenAPI enum reference);
// "type" picks the adapter. "enabled": false disables without deleting
// credentials (absent = enabled).
type Channel struct {
	ID      string
	Type    string
	Enabled bool
	Adapter adapter.Adapter
}

// ChannelSet is the immutable parsed channels file; swapped atomically.
type ChannelSet struct {
	byID map[string]*Channel
}

func (s *ChannelSet) Get(id string) (*Channel, bool) {
	c, ok := s.byID[id]
	return c, ok
}

// IDs returns all channel ids, sorted (feeds the OpenAPI enum later).
func (s *ChannelSet) IDs() []string {
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Enabled returns the enabled channels, sorted by id.
func (s *ChannelSet) Enabled() []*Channel {
	var chs []*Channel
	for _, id := range s.IDs() {
		if c := s.byID[id]; c.Enabled {
			chs = append(chs, c)
		}
	}
	return chs
}

type channelEnvelope struct {
	Type    string `toml:"type"`
	Enabled *bool  `toml:"enabled"`
}

// maxChannelIDLen keeps an id usable as a path component on every filesystem
// the hub might land on, with room to spare.
const maxChannelIDLen = 64

// validChannelID rejects any id that isn't a single, safe path component.
// The id is the spool directory name (`spool/<id>/`), so ".." would point a
// channel's spool at the config directory — whose files the drain scan then
// parses as zero-valued, long-expired items and deletes, taking keys.toml and
// channels.toml with them. A separator anywhere is enough to strand a spool
// outside the tree the engine reconciles.
func validChannelID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("channel id must not be empty")
	case id == "." || id == "..":
		return fmt.Errorf("channel id %q is a path reference, not a name", id)
	case len(id) > maxChannelIDLen:
		return fmt.Errorf("channel id is %d bytes; max %d", len(id), maxChannelIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("channel id may only contain letters, digits, '-', '_' and '.' (it names the spool directory)")
		}
	}
	return nil
}

// LoadChannels parses and validates channels.toml, constructing every enabled
// instance's adapter so a bad config block fails the whole load
// (validate-before-swap keeps the previous set alive).
//
// A disabled instance is type-checked but NOT constructed: enabled = false is
// how an operator parks a channel that has gone bad, usually while stripping
// its now-stale settings, and that edit must not take the whole file — and
// with it every healthy channel — down with it.
func LoadChannels(path string) (*ChannelSet, error) {
	// Primitive defers each block's decode until we know which adapter owns
	// it. The metadata tracks what every deferred decode consumed, so the
	// unknown-key check at the end still covers adapter settings.
	var entries map[string]toml.Primitive
	md, err := toml.DecodeFile(path, &entries)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	set := &ChannelSet{byID: make(map[string]*Channel, len(entries))}
	for id, prim := range entries {
		if err := validChannelID(id); err != nil {
			return nil, fmt.Errorf("%s: channel %q: %w", path, id, err)
		}
		var env channelEnvelope
		if err := md.PrimitiveDecode(prim, &env); err != nil {
			return nil, fmt.Errorf("%s: channel %q: %w", path, id, err)
		}
		if env.Type == "" {
			return nil, fmt.Errorf("%s: channel %q: missing type", path, id)
		}
		ch := &Channel{ID: id, Type: env.Type, Enabled: env.Enabled == nil || *env.Enabled}
		if !ch.Enabled {
			// Typo-check the type name only — cheap, and it still catches the
			// edit that would fail on re-enable. Absorb the rest of the block
			// so its settings don't read as unknown keys; a parked channel is
			// deliberately not validated.
			if !adapter.Known(env.Type) {
				return nil, fmt.Errorf("%s: channel %q: unknown adapter type %q", path, id, env.Type)
			}
			var parked map[string]any
			if err := md.PrimitiveDecode(prim, &parked); err != nil {
				return nil, fmt.Errorf("%s: channel %q: %w", path, id, err)
			}
			set.byID[id] = ch
			continue
		}
		decode := func(v any) error { return md.PrimitiveDecode(prim, v) }
		a, err := adapter.New(env.Type, id, decode)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ch.Adapter = a
		set.byID[id] = ch
	}
	// Runs after every deferred decode, so it catches a typo in an adapter's
	// own settings as well as a stray top-level key.
	if err := rejectUnknown(path, md); err != nil {
		return nil, err
	}
	return set, nil
}

// All returns every channel, enabled or not, sorted by id. The outbox needs
// the disabled ones too: a disabled channel keeps its spool (disabling is a
// pause, not a purge), which is only distinguishable from a *removed*
// channel if the engine is told about both.
func (s *ChannelSet) All() []*Channel {
	chs := make([]*Channel, 0, len(s.byID))
	for _, id := range s.IDs() {
		chs = append(chs, s.byID[id])
	}
	return chs
}
