package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// Caller is one unified caller entry: bearer key(s) + permission boundary.
// "key" accepts a string or an array — two concurrently-valid keys make
// rotation add → flip → remove with no outage window.
type Caller struct {
	ID       string
	Keys     []string
	Channels []string
	// Parsed-but-inert until their features land (per-key caps, hooks).
	MaxPerHour int
	URLTokens  []string
	Defaults   map[string]string
}

type callerTOML struct {
	Key        flexStrings       `toml:"key"`
	URLToken   flexStrings       `toml:"url_token"`
	Channels   []string          `toml:"channels"`
	MaxPerHour int               `toml:"max_per_hour"`
	Defaults   map[string]string `toml:"defaults"`
}

// flexStrings accepts key = "x" or key = ["x", "y"].
type flexStrings []string

func (f *flexStrings) UnmarshalTOML(v any) error {
	switch t := v.(type) {
	case string:
		*f = []string{t}
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("expected a string or an array of strings, got %T inside the array", e)
			}
			out = append(out, s)
		}
		*f = out
		return nil
	default:
		return fmt.Errorf("expected a string or an array of strings, got %T", v)
	}
}

// Keyring is the immutable parsed keys file; swapped atomically on reload.
type Keyring struct {
	byKey map[string]*Caller
}

func (k *Keyring) Lookup(key string) (*Caller, bool) {
	c, ok := k.byKey[key]
	return c, ok
}

const minKeyLen = 16 // bytes of the string; issued keys are ≥16 bytes of entropy

func LoadKeys(path string) (*Keyring, error) {
	var entries map[string]callerTOML
	md, err := toml.DecodeFile(path, &entries)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := rejectUnknown(path, md); err != nil {
		return nil, err
	}

	ring := &Keyring{byKey: make(map[string]*Caller)}
	for id, e := range entries {
		if len(e.Key) == 0 && len(e.URLToken) == 0 {
			return nil, fmt.Errorf("%s: caller %q has no key or url_token", path, id)
		}
		if e.Channels == nil {
			return nil, fmt.Errorf("%s: caller %q has no channels list", path, id)
		}
		c := &Caller{
			ID:         id,
			Keys:       e.Key,
			Channels:   e.Channels,
			MaxPerHour: e.MaxPerHour,
			URLTokens:  e.URLToken,
			Defaults:   e.Defaults,
		}
		for _, key := range e.Key {
			if len(key) < minKeyLen {
				return nil, fmt.Errorf("%s: caller %q has a key shorter than %d chars", path, id, minKeyLen)
			}
			if prev, dup := ring.byKey[key]; dup {
				return nil, fmt.Errorf("%s: key shared by callers %q and %q", path, prev.ID, id)
			}
			ring.byKey[key] = c
		}
	}
	return ring, nil
}

// Permitted reports whether the caller's permission boundary includes ch.
func (c *Caller) Permitted(ch string) bool {
	for _, p := range c.Channels {
		if p == ch {
			return true
		}
	}
	return false
}
