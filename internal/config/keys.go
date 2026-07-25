package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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

type callerJSON struct {
	Key        flexStrings       `json:"key"`
	URLToken   flexStrings       `json:"urlToken"`
	Channels   []string          `json:"channels"`
	MaxPerHour int               `json:"maxPerHour"`
	Defaults   map[string]string `json:"defaults"`
}

// flexStrings unmarshals "x" or ["x","y"].
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = []string{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err != nil {
		return fmt.Errorf("expected string or array of strings: %w", err)
	}
	*f = ss
	return nil
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
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if dup := firstDuplicateKey(raw); dup != "" {
		return nil, fmt.Errorf("%s: caller %q appears twice (JSON keeps only the last, silently dropping the first block's keys)", path, dup)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries map[string]callerJSON
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	ring := &Keyring{byKey: make(map[string]*Caller)}
	for id, e := range entries {
		if len(e.Key) == 0 && len(e.URLToken) == 0 {
			return nil, fmt.Errorf("%s: caller %q has no key or urlToken", path, id)
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
