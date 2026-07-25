package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// ntfy publishes via ntfy's JSON endpoint (POST to the server root, topic in
// the body), not header-mapped publishing: titles with emoji/unicode break
// HTTP headers. The topic token stays in the auth header.

const ntfyMaxMessageBytes = 4096

func init() {
	Register("ntfy", func(id string, cfg json.RawMessage) (Adapter, error) {
		return newNtfy(id, cfg)
	})
}

type ntfyConfig struct {
	// Type and Enabled belong to the instance envelope; tolerated here so
	// the factory can decode the whole block strictly.
	Type    string `json:"type"`
	Enabled *bool  `json:"enabled"`

	Server string `json:"server"`
	Topic  string `json:"topic"`
	Token  string `json:"token"`
}

type ntfyAdapter struct {
	id     string
	server string
	topic  string
	token  string
	client *http.Client
}

func newNtfy(id string, cfg json.RawMessage) (*ntfyAdapter, error) {
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	var c ntfyConfig
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("channel %q (ntfy): %w", id, err)
	}
	if c.Server == "" {
		c.Server = "https://ntfy.sh"
	}
	if c.Topic == "" {
		return nil, fmt.Errorf("channel %q (ntfy): topic is required", id)
	}
	return &ntfyAdapter{
		id:     id,
		server: strings.TrimRight(c.Server, "/"),
		topic:  c.Topic,
		token:  c.Token,
		client: &http.Client{}, // per-attempt timeout comes via ctx
	}, nil
}

var ntfyPriorities = map[notify.Priority]int{
	notify.PriorityLow:     2,
	notify.PriorityDefault: 3,
	notify.PriorityHigh:    4,
	notify.PriorityUrgent:  5,
}

func (a *ntfyAdapter) Send(ctx context.Context, n notify.Notification) error {
	body := map[string]any{
		"topic":    a.topic,
		"title":    n.Title,
		"message":  notify.TruncateBytes(n.Message, ntfyMaxMessageBytes, "…[truncated]"),
		"priority": ntfyPriorities[n.Priority],
	}
	if len(n.Tags) > 0 {
		body["tags"] = n.Tags
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Permanent("encode: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.server, bytes.NewReader(payload))
	if err != nil {
		return Permanent("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return Retryable("ntfy: %v", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return RateLimited(time.Now().Add(ParseRetryAfter(resp.Header.Get("Retry-After"), 30*time.Second)),
			"ntfy: 429 rate limited")
	case resp.StatusCode >= 500:
		return Retryable("ntfy: %d %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	default:
		return Permanent("ntfy: %d %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

// ParseRetryAfter handles both delta-seconds and HTTP-date forms. The
// fallback is used when the header is missing, unparseable, or asks for a
// non-positive delay — "Retry-After: 0" and dates already in the past (a few
// seconds of clock skew is enough) must not turn into "retry immediately"
// against the server that just throttled us.
func ParseRetryAfter(v string, fallback time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if d := time.Duration(secs) * time.Second; d > 0 {
			return d
		}
		return fallback
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return fallback
	}
	return fallback
}
