package notify

import (
	"strings"
	"testing"
)

func TestSanitizeTitleStripsAllControls(t *testing.T) {
	in := "Backup\x1b[31m failed\non betty\ttoday 🚨"
	got := SanitizeTitle(in)
	want := "Backup[31m failedon bettytoday 🚨"
	if got != want {
		t.Errorf("SanitizeTitle(%q) = %q, want %q", in, got, want)
	}
}

func TestSanitizeMessageKeepsNewlinesAndTabs(t *testing.T) {
	in := "line1\nline2\tend\x1b[0m\x07"
	got := SanitizeMessage(in)
	want := "line1\nline2\tend[0m"
	if got != want {
		t.Errorf("SanitizeMessage(%q) = %q, want %q", in, got, want)
	}
}

func TestParsePriority(t *testing.T) {
	for in, want := range map[string]Priority{
		"":        PriorityDefault,
		"URGENT":  PriorityUrgent,
		" High ":  PriorityHigh,
		"default": PriorityDefault,
		"low":     PriorityLow,
	} {
		got, err := ParsePriority(in)
		if err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParsePriority("shouty"); err == nil {
		t.Error("ParsePriority(shouty) should error")
	}
}

func TestValidateCaps(t *testing.T) {
	n := Notification{Title: "t", Message: strings.Repeat("x", MaxMessageLen+1)}
	if err := n.Validate(); err == nil {
		t.Error("over-length message should fail validation")
	}
	n = Notification{Title: "", Message: "m"}
	if err := n.Validate(); err == nil {
		t.Error("empty title should fail validation")
	}
}

func TestTruncateBytesRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes each
	got := TruncateBytes(s, 51, "…")
	if !strings.HasSuffix(got, "…") || len(got) > 51 {
		t.Errorf("truncated to %d bytes: %q", len(got), got[len(got)-8:])
	}
	if TruncateBytes("short", 100, "…") != "short" {
		t.Error("under-limit string must pass through")
	}
}

// The request id is the delivery log's only correlation key and the outbox
// registry's waiter key: a collision cross-wires two in-flight requests'
// outcomes. 4 bytes collided by the birthday bound after ~65k requests.
func TestRequestIDEntropy(t *testing.T) {
	const want = 2 + 32 // "r_" + 16 bytes hex-encoded
	id := NewRequestID()
	if len(id) != want {
		t.Errorf("NewRequestID() = %q (%d chars), want %d — at least 16 bytes of entropy", id, len(id), want)
	}
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := NewRequestID()
		if seen[id] {
			t.Fatalf("duplicate request id %q after %d draws", id, i)
		}
		seen[id] = true
	}
}

func TestSanitizeTags(t *testing.T) {
	got := SanitizeTags([]string{"deploy", "sev\x1b[31m1", "\x00\x07", "ok"})
	want := []string{"deploy", "sev[31m1", "ok"}
	if len(got) != len(want) {
		t.Fatalf("SanitizeTags = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d = %q, want %q", i, got[i], want[i])
		}
	}
	if SanitizeTags(nil) != nil {
		t.Error("nil tags must stay nil")
	}
}
