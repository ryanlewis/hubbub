package adapter

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// exec is the Unix escape hatch and the hub's no-rebuild extension point: a new
// channel becomes a script plus a config block, so the long tail (SMS gateways,
// a curl at a Discord webhook, a lamp on a GPIO pin) never needs a Go file.
//
// A compiled adapter earns its place when the hub should own formatting and
// limits — ntfy's JSON-endpoint quirks, fit-to-channel truncation, Retry-After
// handling. Everything else belongs here.
//
// Three rules make it safe to hand an operator's script a caller's text:
//
//   - The configured command is argv, executed directly. Never `sh -c`, and
//     notification content never reaches argv — so there is nowhere for an
//     injection to live. Shell semantics are available by making the configured
//     thing a shell script.
//   - The payload is data on stdin, plus NOTIFY_* for one-liners. Scripts must
//     treat it as data (no eval): a leaked API key yields attacker-controlled
//     text, never command choice.
//   - Exit codes are the retry contract, the way status codes are for HTTP
//     adapters.

const (
	// execTempFail is EX_TEMPFAIL from sysexits.h — the one exit code that
	// means "ask me again". Chosen over an invented number because scripts can
	// already get it from /usr/include/sysexits.h and it is what a Unix-shaped
	// tool expects.
	execTempFail = 75

	// execStderrCapture bounds what is held in memory from a script that
	// decides to write a core dump to stderr; execReasonBytes bounds what of
	// that reaches an HTTP response and a delivery-log line.
	execStderrCapture = 4 << 10
	execReasonBytes   = 512

	// execWaitDelay bounds how long Wait blocks on I/O after the process is
	// killed. Killing the group should close every pipe, so this only fires if
	// something escaped it.
	execWaitDelay = 2 * time.Second

	// execDefaultPath is used when hubbub's own PATH is empty, which is exactly
	// what a bare systemd unit gives it.
	execDefaultPath = "/usr/local/bin:/usr/bin:/bin"
)

func init() {
	// Registered as an executor: its settings name a command, so /admin will
	// not write one unless the deployment opted in.
	RegisterExecutor("exec", func(id string, decode Decode) (Adapter, error) {
		return newExec(id, decode)
	})
}

type execConfig struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

type execAdapter struct {
	id      string
	command string
	args    []string
	path    string
}

func newExec(id string, decode Decode) (*execAdapter, error) {
	var c execConfig
	if err := decode(&c); err != nil {
		return nil, fmt.Errorf("channel %q (exec): %w", id, err)
	}
	if c.Command == "" {
		return nil, fmt.Errorf("channel %q (exec): command is required", id)
	}
	// A bare name would be resolved against the service unit's PATH at send
	// time, not the operator's login PATH at edit time — so the failure lands
	// as a permanent per-message error hours later instead of a config error
	// now. Absolute paths make "which binary is this" answerable from the file.
	if !filepath.IsAbs(c.Command) {
		return nil, fmt.Errorf("channel %q (exec): command must be an absolute path, got %q", id, c.Command)
	}
	// Deliberately not stat'd here. A missing script would fail the whole
	// channels.toml load, and validate-before-swap would then keep the previous
	// config — so a script deployed a minute after its channel, or one on a
	// filesystem that mounts late, would silently discard an unrelated edit
	// elsewhere in the file. It is a send-time permanent failure instead, which
	// names the path and settles that one message.
	return &execAdapter{
		id:      id,
		command: c.Command,
		args:    c.Args,
		path:    cmp.Or(os.Getenv("PATH"), execDefaultPath),
	}, nil
}

// execPayload is the stdin contract, spelled out here rather than marshalling
// notify.Notification directly: the internal type gains fields as the hub
// grows, and every one of them would otherwise silently change what an
// operator's script is handed.
//
// html is absent on purpose. exec channels are text channels by construction —
// the field is up to 128 KB of markup a shell script has no way to render, and
// piping it into one that only reads $1 wastes the spool round-trip.
type execPayload struct {
	Channel   string    `json:"channel"`
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Priority  string    `json:"priority"`
	Tags      []string  `json:"tags"`
	CallerID  string    `json:"callerId"`
	RequestID string    `json:"requestId"`
	CreatedAt time.Time `json:"createdAt"`
}

func (a *execAdapter) Send(ctx context.Context, n notify.Notification) error {
	tags := n.Tags
	if tags == nil {
		// Always an array, never null: `.tags[]` in a jq one-liner should not
		// need to care whether the caller sent any.
		tags = []string{}
	}
	payload, err := json.Marshal(execPayload{
		Channel:   a.id,
		Title:     n.Title,
		Message:   n.Message,
		Priority:  string(n.Priority),
		Tags:      tags,
		CallerID:  n.CallerID,
		RequestID: n.RequestID,
		// CreatedAt is when the message was accepted, not when the script ran.
		// A paced drain after an outage delivers hours-old messages, and a
		// script that says so ("queued at …") beats one that implies now.
		CreatedAt: n.CreatedAt,
	})
	if err != nil {
		return Permanent("exec: encode payload: %v", err)
	}

	cmd := exec.CommandContext(ctx, a.command, a.args...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = a.env(n)
	stderr := &cappedBuffer{limit: execStderrCapture}
	cmd.Stderr = stderr
	// Stdout is left nil, which os/exec wires to /dev/null: a chatty script
	// would otherwise fill a pipe nobody drains and block forever. Diagnostics
	// belong on stderr, where they become the failure reason.

	setProcessGroup(cmd)
	// The per-attempt timeout has to reach the whole tree, not just the child:
	// a script that shelled out to a curl against a black-holed host is exactly
	// the case this exists for, and killing only the script leaves the curl
	// holding the channel's pipes.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	// Backstop for anything that escaped the group kill — Wait would otherwise
	// block on an inherited pipe for as long as a grandchild cares to hold it.
	cmd.WaitDelay = execWaitDelay

	runErr := cmd.Run()
	reason := execReason(stderr.String())

	if runErr == nil {
		return nil
	}
	// The parent context ending is a shutdown or a config reload, and the
	// worker reads that as "no verdict" and leaves the message spooled. Only
	// the per-attempt deadline is this adapter's to classify, and a script that
	// ran long is the textbook retryable failure.
	if ctx.Err() != nil {
		return Retryable("exec: %s timed out%s", filepath.Base(a.command), withReason(reason))
	}

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		// Never started: not found, not executable, not a directory. All of
		// them are config errors that retrying for a TTL cannot fix.
		return Permanent("exec: %v", runErr)
	}
	code := ee.ExitCode()
	switch {
	case code < 0:
		// Signalled by something other than us — the OOM killer, an operator's
		// pkill, a crash. That is a statement about the host, not about the
		// message, so it earns another go.
		return Retryable("exec: %s %s%s", filepath.Base(a.command), ee.String(), withReason(reason))
	case code == execTempFail:
		// No rate-limited kind here: an exit code carries no not-before, so
		// there is nothing to honour and the generic backoff is the honest
		// answer. A script that knows it is throttled should sleep or exit 75.
		return Retryable("exec: %s exit %d (EX_TEMPFAIL)%s", filepath.Base(a.command), code, withReason(reason))
	default:
		return Permanent("exec: %s exit %d%s", filepath.Base(a.command), code, withReason(reason))
	}
}

// env is minimal on purpose: hubbub's own environment can hold credentials
// (channel tokens arrive by file, but a supervisor's environment is not the
// hub's to audit), and a script that needs a secret should read it from a file
// it owns. PATH is inherited because shebangs and subcommands need one.
//
// The NOTIFY_* copies exist so a one-liner never has to parse JSON. They are
// safe to build unconditionally because ingest caps title at 256 bytes, message
// at 4096 and tags at 16×64 — the whole environment stays a few kilobytes,
// nowhere near ARG_MAX.
func (a *execAdapter) env(n notify.Notification) []string {
	return []string{
		"PATH=" + a.path,
		"NOTIFY_CHANNEL=" + a.id,
		"NOTIFY_TITLE=" + n.Title,
		"NOTIFY_MESSAGE=" + n.Message,
		"NOTIFY_PRIORITY=" + string(n.Priority),
		"NOTIFY_TAGS=" + strings.Join(n.Tags, ","),
		"NOTIFY_CALLER_ID=" + n.CallerID,
		"NOTIFY_REQUEST_ID=" + n.RequestID,
	}
}

// execReason folds a script's stderr into one line fit for a JSON response and
// a delivery-log line. Sanitisation normally happens once at ingest, but this
// text is the script's, not the caller's, and it has never been through it.
func execReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	s = notify.SanitizeTitle(s)
	s = strings.Join(strings.Fields(s), " ")
	return notify.TruncateBytes(s, execReasonBytes, "…[truncated]")
}

func withReason(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// cappedBuffer keeps the first limit bytes and discards the rest, while still
// reporting a full write. os/exec copies stderr in its own goroutine, so
// refusing bytes here would stall that copy and, with it, Wait.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
