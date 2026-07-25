// Package dlog is the JSONL delivery log: one line per notification at
// accept time, a second terminal line for queued outcomes, plus auth-failure
// and rate-cap lines. Always encoder-built (never hand-concatenated);
// content is already control-char-stripped at ingest.
package dlog

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

type Record struct {
	Time      time.Time         `json:"ts"`
	Kind      string            `json:"kind"` // request | terminal | auth_fail
	RequestID string            `json:"requestId,omitempty"`
	CallerID  string            `json:"callerId,omitempty"`
	Result    string            `json:"result,omitempty"`
	Channels  map[string]string `json:"channels,omitempty"`
	Title     string            `json:"title,omitempty"`
	MsgBytes  int               `json:"msgBytes,omitempty"`
	HTMLBytes int               `json:"htmlBytes,omitempty"`
	Priority  string            `json:"priority,omitempty"`
	Channel   string            `json:"channel,omitempty"` // terminal lines
	Outcome   string            `json:"outcome,omitempty"` // terminal lines
	Peer      string            `json:"peer,omitempty"`    // auth_fail lines
	ClaimedIP string            `json:"claimedIp,omitempty"`
	Detail    string            `json:"detail,omitempty"`
}

type Logger struct {
	mu sync.Mutex
	f  *os.File
}

func Open(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

func (l *Logger) Append(r Record) {
	if l == nil {
		return
	}
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	line, err := json.Marshal(r)
	if err != nil {
		slog.Error("delivery log encode failed", "err", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		slog.Error("delivery log write failed", "err", err)
	}
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
