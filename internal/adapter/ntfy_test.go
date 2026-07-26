package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ryanlewis/hubbub/internal/notify"
)

// cfgFrom builds the Decode an adapter factory is handed. The config package
// supplies the real one; adapters never see the format themselves, so the
// test just needs something that fills their struct.
func cfgFrom(doc string) Decode {
	return func(v any) error {
		_, err := toml.Decode(doc, v)
		return err
	}
}

func ntfyFor(t *testing.T, srvURL string) Adapter {
	t.Helper()
	cfg := "server = \"" + srvURL + "\"\ntopic = \"tst\"\ntoken = \"tk_x\"\n"
	a, err := New("ntfy", "ntfy", cfgFrom(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func note() notify.Notification {
	return notify.Notification{
		Title: "hi", Message: "body", Priority: notify.PriorityHigh,
		Tags: []string{"a"}, RequestID: "r_test",
	}
}

func TestNtfySendOK(t *testing.T) {
	var got map[string]any
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer ts.Close()

	if err := ntfyFor(t, ts.URL).Send(context.Background(), note()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got["topic"] != "tst" || got["title"] != "hi" || got["priority"] != float64(4) {
		t.Errorf("payload = %v", got)
	}
	if auth != "Bearer tk_x" {
		t.Errorf("auth header = %q", auth)
	}
}

func TestNtfyClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   Kind
	}{
		{429, KindRateLimited},
		{500, KindRetryable},
		{400, KindPermanent},
		{403, KindPermanent},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.status == 429 {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(tc.status)
		}))
		err := ntfyFor(t, ts.URL).Send(context.Background(), note())
		ts.Close()
		se, ok := err.(*SendError)
		if !ok {
			t.Fatalf("status %d: err = %v (not *SendError)", tc.status, err)
		}
		if se.Kind != tc.kind {
			t.Errorf("status %d: kind = %v, want %v", tc.status, se.Kind, tc.kind)
		}
		if tc.status == 429 {
			d := time.Until(se.NotBefore)
			if d < 110*time.Second || d > 130*time.Second {
				t.Errorf("429 NotBefore in %v, want ~120s", d)
			}
		}
	}
}

// Go's default redirect policy turns a 301 into a GET and drops the body, so a
// followed redirect publishes nothing and hands back the landing page's 200 —
// which the adapter would report as delivered. Silent loss, with a success line
// in the delivery log.
func TestNtfyRefusesToFollowARedirect(t *testing.T) {
	var landed bool
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	err := ntfyFor(t, redirector.URL).Send(context.Background(), note())
	se, ok := err.(*SendError)
	if !ok {
		t.Fatalf("err = %v, want a *SendError — a redirect must never read as success", err)
	}
	if se.Kind != KindPermanent {
		t.Errorf("kind = %v, want KindPermanent: a redirecting server= is a config error, not a blip", se.Kind)
	}
	if !strings.Contains(se.Reason, "server=") {
		t.Errorf("reason = %q, want it to say what to fix", se.Reason)
	}
	if landed {
		t.Error("the publish was forwarded to the redirect target")
	}
}

func TestNtfyTruncatesLongMessage(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer ts.Close()

	n := note()
	n.Message = strings.Repeat("x", 10000)
	if err := ntfyFor(t, ts.URL).Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	msg := got["message"].(string)
	if len(msg) > 4096 || !strings.HasSuffix(msg, "…[truncated]") {
		t.Errorf("message len %d, suffix %q", len(msg), msg[len(msg)-20:])
	}
}

func TestNtfyRejectsMissingTopic(t *testing.T) {
	if _, err := New("ntfy", "x", cfgFrom("")); err == nil {
		t.Error("missing topic must fail validation")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := ParseRetryAfter("60", time.Second); d != 60*time.Second {
		t.Errorf("delta-seconds: %v", d)
	}
	if d := ParseRetryAfter("", 7*time.Second); d != 7*time.Second {
		t.Errorf("fallback: %v", d)
	}
	if d := ParseRetryAfter("garbage", 7*time.Second); d != 7*time.Second {
		t.Errorf("garbage fallback: %v", d)
	}
}

// A Retry-After of 0, or an HTTP-date already in the past (a few seconds of
// clock skew is enough), must not mean "retry immediately" against the server
// that just throttled us.
func TestParseRetryAfterNonPositiveFallsBack(t *testing.T) {
	const fallback = 30 * time.Second
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	for _, v := range []string{"0", "-5", past, "", "garbage"} {
		if got := ParseRetryAfter(v, fallback); got != fallback {
			t.Errorf("ParseRetryAfter(%q) = %v, want the %v fallback", v, got, fallback)
		}
	}
	if got := ParseRetryAfter("12", fallback); got != 12*time.Second {
		t.Errorf("ParseRetryAfter(\"12\") = %v, want 12s (a real delay is still honoured)", got)
	}
}
