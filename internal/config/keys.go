package config

import (
	"fmt"
	"os"
	"slices"
	"sort"

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

// maxCallerIDLen matches the channel-id cap; the id is a label, not a payload.
const maxCallerIDLen = 64

// ValidCallerID rejects any id that isn't a bare token.
//
// TOML would happily accept ["ops.team"], but a dotted id is indistinguishable
// at a glance from [ops.team] — a sub-table, an entirely different thing — and
// the admin dashboard locates a caller by matching its header in the file's
// source text. Constraining ids here means that ambiguity can never arise.
// The id also lands in every delivery-log line, so keeping it to plain
// characters is worth something on its own.
//
// Exported because the dashboard writes `[<id>]` into keys.toml by string
// interpolation, so it has to be able to ask this question *before* the file is
// parsed — a caller id carrying a newline would otherwise splice a whole second
// table, with a key of the submitter's choosing, into a file that then parses
// perfectly.
func ValidCallerID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("caller id must not be empty")
	case len(id) > maxCallerIDLen:
		return fmt.Errorf("caller id is %d bytes; max %d", len(id), maxCallerIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("caller id may only contain letters, digits, '-' and '_'")
		}
	}
	return nil
}

// LoadKeys reads and parses a keys file.
func LoadKeys(path string) (*Keyring, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKeys(src, path)
}

// ParseKeys parses a keys file held in memory, reporting errors against name.
//
// Split out from LoadKeys so a candidate edit can be validated without going
// near the filesystem. Validating through a temp file would leave a 0600 file
// full of live bearer keys sitting in the config directory whenever the
// process died between writing it and unlinking it — and it would report
// failures against a path the operator has never seen.
func ParseKeys(src []byte, name string) (*Keyring, error) {
	var entries map[string]callerTOML
	md, err := toml.Decode(string(src), &entries)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := rejectUnknown(name, md); err != nil {
		return nil, err
	}

	ring := &Keyring{byKey: make(map[string]*Caller)}
	for id, e := range entries {
		if err := ValidCallerID(id); err != nil {
			return nil, fmt.Errorf("%s: caller %q: %w", name, id, err)
		}
		if len(e.Key) == 0 && len(e.URLToken) == 0 {
			return nil, fmt.Errorf("%s: caller %q has no key or url_token", name, id)
		}
		if e.Channels == nil {
			return nil, fmt.Errorf("%s: caller %q has no channels list", name, id)
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
				return nil, fmt.Errorf("%s: caller %q has a key shorter than %d chars", name, id, minKeyLen)
			}
			if prev, dup := ring.byKey[key]; dup {
				return nil, fmt.Errorf("%s: key shared by callers %q and %q", name, prev.ID, id)
			}
			ring.byKey[key] = c
		}
	}
	return ring, nil
}

// Callers returns every caller, sorted by id and deduplicated — the ring is
// indexed by key, so a caller mid-rotation appears under two of them. The
// admin dashboard needs the caller list, not the key list.
func (k *Keyring) Callers() []*Caller {
	seen := make(map[string]*Caller, len(k.byKey))
	for _, c := range k.byKey {
		seen[c.ID] = c
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Caller, 0, len(ids))
	for _, id := range ids {
		out = append(out, seen[id])
	}
	return out
}

// Permitted reports whether the caller's permission boundary includes ch.
func (c *Caller) Permitted(ch string) bool {
	return slices.Contains(c.Channels, ch)
}
