//go:build unix

package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ryanlewis/hubbub/internal/notify"
)

// execFor writes body to an executable file and builds an exec channel around
// it. The script's own directory is interpolated as $dir, since the adapter
// passes no environment a test could smuggle a path through — which is itself
// part of the contract.
func execFor(t *testing.T, body string, args ...string) (Adapter, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "channel.sh")
	body = strings.ReplaceAll(body, "$dir", dir)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "command = " + strconv.Quote(script) + "\n"
	if len(args) > 0 {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = strconv.Quote(a)
		}
		cfg += "args = [" + strings.Join(quoted, ", ") + "]\n"
	}
	a, err := New("exec", "sms", cfgFrom(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return a, dir
}

func execNote() notify.Notification {
	return notify.Notification{
		Title: "disk full", Message: "root at 98%", Priority: notify.PriorityHigh,
		Tags: []string{"warn", "host"}, CallerID: "cron", RequestID: "r_test",
		CreatedAt: time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
	}
}

func TestExecSendsPayloadOnStdin(t *testing.T) {
	a, dir := execFor(t, "#!/bin/sh\ncat > $dir/payload.json\n")
	if err := a.Send(context.Background(), execNote()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got execPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("stdin was not valid JSON: %v", err)
	}
	if got.Channel != "sms" || got.Title != "disk full" || got.Message != "root at 98%" {
		t.Errorf("payload = %+v", got)
	}
	if got.Priority != "high" || got.CallerID != "cron" || got.RequestID != "r_test" {
		t.Errorf("payload = %+v", got)
	}
	if !got.CreatedAt.Equal(execNote().CreatedAt) {
		t.Errorf("createdAt = %v, want the enqueue time — a paced drain delivers old messages", got.CreatedAt)
	}
	if strings.Join(got.Tags, ",") != "warn,host" {
		t.Errorf("tags = %v", got.Tags)
	}
}

// A caller sending no tags must still hand the script an array: `.tags[]` in a
// one-liner should not have to care.
func TestExecPayloadTagsAreNeverNull(t *testing.T) {
	a, dir := execFor(t, "#!/bin/sh\ncat > $dir/payload.json\n")
	n := execNote()
	n.Tags = nil
	if err := a.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "payload.json"))
	if !strings.Contains(string(raw), `"tags":[]`) {
		t.Errorf("payload = %s, want an empty array", raw)
	}
}

func TestExecEnvIsMinimalAndCarriesNotifyVars(t *testing.T) {
	t.Setenv("HUBBUB_SECRET", "must-not-leak")
	a, dir := execFor(t, "#!/bin/sh\nenv > $dir/env.txt\n")
	if err := a.Send(context.Background(), execNote()); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"NOTIFY_CHANNEL=sms", "NOTIFY_TITLE=disk full", "NOTIFY_MESSAGE=root at 98%",
		"NOTIFY_PRIORITY=high", "NOTIFY_TAGS=warn,host",
		"NOTIFY_CALLER_ID=cron", "NOTIFY_REQUEST_ID=r_test",
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env is missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), "HUBBUB_SECRET") {
		t.Error("hubbub's own environment reached the script — it may hold credentials")
	}
	if !strings.Contains(string(env), "PATH=") {
		t.Error("PATH must be inherited: shebangs and subcommands need one")
	}
}

// The injection guarantee: notification content is data, and the only thing on
// argv is what the operator configured.
func TestExecContentNeverReachesArgv(t *testing.T) {
	a, dir := execFor(t,
		"#!/bin/sh\nprintf '%s\\n' \"$@\" > $dir/argv.txt\ncat > $dir/payload.json\n",
		"--to", "+15550000")

	n := execNote()
	n.Title = `"; rm -rf / #`
	n.Message = "$(id) `id`"
	if err := a.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	argv, _ := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if got := strings.TrimRight(string(argv), "\n"); got != "--to\n+15550000" {
		t.Errorf("argv = %q, want only the configured args", got)
	}
	payload, _ := os.ReadFile(filepath.Join(dir, "payload.json"))
	if !strings.Contains(string(payload), "rm -rf") {
		t.Error("the title should still have arrived intact, as data on stdin")
	}
}

func TestExecClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		err    bool
		kind   Kind
	}{
		{name: "exit 0 delivers", script: "#!/bin/sh\nexit 0\n"},
		{
			name:   "EX_TEMPFAIL retries",
			script: "#!/bin/sh\necho 'gateway busy' >&2\nexit 75\n",
			err:    true, kind: KindRetryable,
		},
		{
			name:   "any other code is permanent",
			script: "#!/bin/sh\necho 'bad recipient' >&2\nexit 1\n",
			err:    true, kind: KindPermanent,
		},
		{
			name:   "a script that ignores stdin still succeeds",
			script: "#!/bin/sh\nexit 0\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := execFor(t, tc.script)
			err := a.Send(context.Background(), execNote())
			if !tc.err {
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
				return
			}
			var se *SendError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (not *SendError)", err)
			}
			if se.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", se.Kind, tc.kind)
			}
		})
	}
}

func TestExecStderrBecomesTheReason(t *testing.T) {
	a, _ := execFor(t, "#!/bin/sh\necho 'carrier rejected: 21610 unsubscribed' >&2\nexit 1\n")
	err := a.Send(context.Background(), execNote())
	if err == nil {
		t.Fatal("want a failure")
	}
	if !strings.Contains(err.Error(), "21610 unsubscribed") {
		t.Errorf("reason = %q, want the script's stderr in it", err)
	}
	if !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("reason = %q, want the exit code in it", err)
	}
}

// The reason lands in a JSON response and a delivery-log line, so a script that
// writes an essay in colour must come back as one bounded, printable line.
func TestExecReasonIsOneBoundedLine(t *testing.T) {
	a, _ := execFor(t, "#!/bin/sh\nprintf 'line one\\n\\033[31mline two\\033[0m\\n' >&2\nfor i in $(seq 1 500); do echo 'padding padding padding' >&2; done\nexit 1\n")
	err := a.Send(context.Background(), execNote())
	if err == nil {
		t.Fatal("want a failure")
	}
	reason := err.Error()
	if strings.ContainsAny(reason, "\n\r\t\x1b") {
		t.Errorf("reason carries control characters: %q", reason)
	}
	if len(reason) > execReasonBytes+128 {
		t.Errorf("reason is %d bytes, want it bounded near %d", len(reason), execReasonBytes)
	}
	if !strings.Contains(reason, "line one") {
		t.Errorf("reason = %q, want the start of stderr kept", reason)
	}
}

// The case exec exists for: a script that shelled out to something hung. The
// timeout must reach the whole process group, or the grandchild outlives the
// attempt and keeps the channel's pipes open.
func TestExecTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	a, dir := execFor(t, "#!/bin/sh\n( sleep 30 ) &\necho $! > $dir/grandchild.pid\nsleep 30\n")

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := a.Send(ctx, execNote())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a timed-out script must not report delivery")
	}
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (not *SendError)", err)
	}
	if se.Kind != KindRetryable {
		t.Errorf("kind = %v, want KindRetryable: a slow script is a blip, not a verdict", se.Kind)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Send took %v — the attempt deadline did not bound it", elapsed)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "grandchild.pid"))
	if err != nil {
		t.Fatalf("the script never recorded its grandchild: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// Reparenting and reaping take a moment; poll rather than race it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // don't leak it into the test run
	t.Errorf("grandchild %d survived the attempt timeout", pid)
}

func TestExecRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct{ name, cfg string }{
		{"no command", ""},
		{"relative command", `command = "notify.sh"`},
		{"bare name on PATH", `command = "curl"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New("exec", "x", cfgFrom(tc.cfg)); err == nil {
				t.Error("want a config error")
			}
		})
	}
}

// Not stat'd at load — a missing script must not fail the whole channels.toml
// and take an unrelated edit down with it — so it surfaces here instead, as a
// permanent failure that names the path.
func TestExecMissingCommandIsPermanent(t *testing.T) {
	a, err := New("exec", "sms", cfgFrom(`command = "/nonexistent/notify.sh"`))
	if err != nil {
		t.Fatalf("a missing script must not fail the config load: %v", err)
	}
	sendErr := a.Send(context.Background(), execNote())
	var se *SendError
	if !errors.As(sendErr, &se) {
		t.Fatalf("err = %v (not *SendError)", sendErr)
	}
	if se.Kind != KindPermanent {
		t.Errorf("kind = %v, want KindPermanent: retrying cannot conjure the file", se.Kind)
	}
	if !strings.Contains(se.Reason, "/nonexistent/notify.sh") {
		t.Errorf("reason = %q, want the path in it", se.Reason)
	}
}
