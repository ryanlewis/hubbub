package config

import (
	"bytes"
	"encoding/json"
)

// firstDuplicateKey reports the first repeated key in a top-level JSON object.
//
// encoding/json decodes duplicate object keys into a map last-wins, silently:
// two "betty" blocks in keys.json become one caller and the first block's
// bearer key vanishes from the keyring on what logs as a successful reload.
// The decoder's token stream is the only place the repeat is still visible.
//
// Returns "" when there is no duplicate, or when raw isn't a JSON object —
// the caller's own strict decode reports malformed input.
func firstDuplicateKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return ""
	}
	seen := make(map[string]bool)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := kt.(string)
		if !ok {
			return ""
		}
		if seen[key] {
			return key
		}
		seen[key] = true
		// Consume the value wholesale, whatever its shape.
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}
