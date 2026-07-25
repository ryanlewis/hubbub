// Package notify holds the notification type and the single ingest
// choke-point for sanitisation and validation.
package notify

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type Priority string

const (
	PriorityLow     Priority = "low"
	PriorityDefault Priority = "default"
	PriorityHigh    Priority = "high"
	PriorityUrgent  Priority = "urgent"
)

// ParsePriority normalises case-insensitive input; empty means default.
func ParsePriority(s string) (Priority, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return PriorityDefault, nil
	case "low":
		return PriorityLow, nil
	case "default":
		return PriorityDefault, nil
	case "high":
		return PriorityHigh, nil
	case "urgent":
		return PriorityUrgent, nil
	default:
		return "", fmt.Errorf("unknown priority %q (low|default|high|urgent)", s)
	}
}

// Ingest caps. Generous by design: adapters truncate fit-to-channel.
const (
	MaxTitleLen   = 256
	MaxMessageLen = 4096
	MaxTags       = 16
	MaxTagLen     = 64
)

type Notification struct {
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Priority  Priority  `json:"priority"`
	Tags      []string  `json:"tags,omitempty"`
	CallerID  string    `json:"callerId"`
	RequestID string    `json:"requestId"`
	CreatedAt time.Time `json:"createdAt"`
}

// SanitizeTitle strips ALL control characters: titles risk landing in HTTP
// headers and email subjects.
func SanitizeTitle(s string) string {
	return stripControl(s, false)
}

// SanitizeMessage keeps \n and \t (multi-line log excerpts are content);
// everything else control-ish is stripped.
func SanitizeMessage(s string) string {
	return stripControl(s, true)
}

// SanitizeTags strips ALL control characters from each tag and drops any tag
// left empty. Tags are treated like titles, not messages: they are short
// identifiers that adapters may map onto HTTP headers, and the exec adapter
// hands them to a subprocess.
func SanitizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if c := stripControl(t, false); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stripControl(s string, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if keepNewlines && (r == '\n' || r == '\t') {
			b.WriteRune(r)
			continue
		}
		// C0 controls, DEL, C1 controls (covers ANSI escapes via ESC).
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Validate checks caps after sanitisation.
func (n *Notification) Validate() error {
	if strings.TrimSpace(n.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if strings.TrimSpace(n.Message) == "" {
		return fmt.Errorf("message is required")
	}
	if len(n.Title) > MaxTitleLen {
		return fmt.Errorf("title exceeds %d bytes", MaxTitleLen)
	}
	if len(n.Message) > MaxMessageLen {
		return fmt.Errorf("message exceeds %d bytes", MaxMessageLen)
	}
	if len(n.Tags) > MaxTags {
		return fmt.Errorf("more than %d tags", MaxTags)
	}
	for _, t := range n.Tags {
		if len(t) > MaxTagLen {
			return fmt.Errorf("tag exceeds %d bytes", MaxTagLen)
		}
	}
	return nil
}

// NewRequestID returns a random id like "r_1a2b…" (crypto/rand). 16 bytes,
// matching the ≥16-byte rule for issued credentials: the id is the delivery
// log's only correlation key and the outbox registry's waiter key, so a
// collision would cross-wire two in-flight requests' outcomes.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for a security-relevant id
		// source elsewhere; for a request id we can still panic loudly.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return "r_" + hex.EncodeToString(b[:])
}

// TruncateBytes cuts s to at most maxBytes, appending marker if it cut,
// without splitting a UTF-8 rune. Used by adapters for fit-to-channel.
func TruncateBytes(s string, maxBytes int, marker string) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}
