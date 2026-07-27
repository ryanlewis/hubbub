// Package adapter defines the channel adapter interface and typed send
// errors — the seam through which the generic outbox worker learns retry
// classification without knowing HTTP status codes from exec exit codes.
package adapter

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

type Kind int

const (
	// KindPermanent: the request won't get better; never retried.
	KindPermanent Kind = iota
	// KindRetryable: timeout / network error / 5xx; retried with backoff
	// until TTL.
	KindRetryable
	// KindRateLimited: upstream 429; terminal per attempt, re-enqueued with
	// NotBefore taken from Retry-After.
	KindRateLimited
)

// SendError is the one error type adapters return (besides nil = delivered).
type SendError struct {
	Kind      Kind
	NotBefore time.Time // only for KindRateLimited
	Reason    string
}

func (e *SendError) Error() string { return e.Reason }

func Permanent(format string, args ...any) *SendError {
	return &SendError{Kind: KindPermanent, Reason: fmt.Sprintf(format, args...)}
}

func Retryable(format string, args ...any) *SendError {
	return &SendError{Kind: KindRetryable, Reason: fmt.Sprintf(format, args...)}
}

func RateLimited(notBefore time.Time, format string, args ...any) *SendError {
	return &SendError{Kind: KindRateLimited, NotBefore: notBefore, Reason: fmt.Sprintf(format, args...)}
}

// Adapter is the one-method interface a channel type implements. The
// runtime around it (serial worker, pacing, cooldown, TTL) is generic.
type Adapter interface {
	Send(ctx context.Context, n notify.Notification) error
}

// Decode fills v from a channel instance's config block. Adapters call it with
// a pointer to their own config struct, so the config file's format stays the
// config package's business — an adapter never imports a parser.
type Decode func(v any) error

// Factory decodes and validates a channel instance's config block.
type Factory func(id string, decode Decode) (Adapter, error)

var registry = map[string]Factory{}

func Register(typeName string, f Factory) {
	registry[typeName] = f
}

// executors are the types whose configuration names a program hubbub will run,
// so editing one chooses what code executes on the box rather than where a
// notification goes. The delivery path treats them like any other adapter; the
// distinction exists for /admin, which gates them behind a config flag so that
// dashboard access is not by itself a shell.
var executors = map[string]bool{}

// RegisterExecutor is Register for such a type. Kept as a separate call rather
// than a field on the factory so that adding one is a deliberate act with a
// name attached, not a struct literal someone can fill in absent-mindedly.
func RegisterExecutor(typeName string, f Factory) {
	Register(typeName, f)
	executors[typeName] = true
}

// IsExecutor reports whether configuring this type amounts to choosing a
// program to run. An unknown type answers false: it configures nothing, and
// callers check Known separately.
func IsExecutor(typeName string) bool { return executors[typeName] }

// Known reports whether a type name has a registered factory. Lets the config
// loader typo-check a disabled channel's type without building its adapter.
func Known(typeName string) bool {
	_, ok := registry[typeName]
	return ok
}

// New builds an adapter instance, validating its config block.
func New(typeName, id string, decode Decode) (Adapter, error) {
	f, ok := registry[typeName]
	if !ok {
		return nil, fmt.Errorf("channel %q: unknown adapter type %q (known: %v)", id, typeName, Types())
	}
	return f(id, decode)
}

func Types() []string {
	ts := make([]string, 0, len(registry))
	for t := range registry {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return ts
}
