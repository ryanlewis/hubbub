# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Self-hosted notification fan-out hub: one authenticated JSON API in, delivery to
the operator's channels (ntfy first) out. Callers are machines — cron jobs,
agents, homelab scripts — so every outcome has to be explicit enough for a
script to branch on. `README.md` is the user-facing contract; treat it as
normative for API shape, config fields and delivery guarantees, and update it
alongside behaviour changes.

## Commands

```sh
go build ./...
go test ./...
go test -race ./...          # workers + atomic config swaps; run before shipping outbox changes
go vet ./...
go run . -config example/hubbub.toml
GOOS=linux GOARCH=amd64 go build -o hubbub .   # deploy build; pure stdlib, no CGO
```

Single test / single package:

```sh
go test ./internal/outbox -run TestEvictOldestWhenFull -v
go test ./internal/httpapi -race
```

`example/*.toml` is not just documentation — `TestShippedExampleConfigLoads`
loads it, so a broken example fails the test suite.

## Hard rules

- **Effectively stdlib.** One dependency, deliberately: `BurntSushi/toml`, for
  config UX (comments in hand-edited operational files). It is itself
  dependency-free. Nothing else goes in without a design change first; the
  planned `exec` adapter is the extension point, not libraries.
- **Platform-agnostic core.** Anything host- or provider-specific is an adapter
  or a config value. No hostnames, no tailnet names, no particular heartbeat
  provider baked into code.
- **Flat files only.** TOML (config, keys, channels), JSONL (delivery log),
  maildir-style spool (outbox). No database — a SQLite revisit is earmarked for
  `/v1/recent` + `/admin`, and nothing else.
- Adapters never see the config format: the factory is handed an
  `adapter.Decode` callback.

## Architecture

Package dependency direction (kept acyclic on purpose):

```
notify  ←  adapter  ←  config
   ↑          ↑          ↑
   └──── outbox ─────────┤
              ↑          │
           httpapi ──────┘
```

`outbox` deliberately does **not** import `config` — `outbox.ChannelRuntime` is
the hand-off type, built in `main.go`.

### Request path (`internal/httpapi/server.go`)

`handleNotify` runs a fixed order that other code depends on: authenticate →
global rate cap → strict JSON decode (`DisallowUnknownFields`) → priority parse
→ channel selection → sanitise + validate → `dispatch`.

Channel selection is the permission boundary. A key's `channels` list *is* its
delivery set; a request's `channels` array narrows and never widens (naming an
unpermitted channel is `403`, not a silent intersection), and an explicit
`"channels": []` is `400` rather than a fall-through to everything.

`dispatch` de-duplicates targets, registers waiters (`Registry.Add`) **before**
enqueueing, then waits out `response_window` and reports whatever is known:
finished attempts concretely, anything still in flight as `queued`. `statusFor`
maps the per-channel map onto 200/202/207/502.

`openapi.json` is embedded and hand-written, but the channel enum, server URL
and version are injected per request (`openapi.go`) — all three are properties
of the deployment, so a checked-in value would be wrong everywhere but here.
`openapi_test.go` is a drift check, not documentation: it replays the spec's
examples through the real mux and pins the documented fields, priority enum and
status codes to the code. Change the request shape and the suite fails until the
spec follows.

### Outbox (`internal/outbox`)

- `Engine` holds `workers` (running) and `wanted` (every configured channel,
  enabled or not). `SetChannels` reconciles: enabled → worker, disabled → stop
  the worker but **keep** the spool (a pause, not a purge), gone from config →
  settle the backlog and remove the spool dir. Disabled channels must still be
  passed in, or the engine can't tell "paused" from "removed".
- One `worker` goroutine per channel instance, strictly serial: per-channel
  ordering is free, cooldown/pacing state lives in one goroutine, and shell-out
  adapters never race themselves.
- `spool`: one JSON file per pending message, named
  `<class>-<enqueue-nanos>-<requestID>.json`, so lexicographic sort *is*
  delivery order (class `0` = test sends jump the queue). Atomic rename is the
  transaction; `claim()` decides which of two racing settlers owns the item.
- Retry classification lives in the adapter, not the worker: adapters return
  `adapter.Permanent/Retryable/RateLimited`, and the worker only knows those
  three kinds.

### Two invariants that most bugs here violate

1. **Every terminal outcome is settled exactly once**, either by resolving a
   waiting handler (`Registry.Resolve` reports whether one consumed it) or by
   writing a `terminal` line to the delivery log. A `202` is a promise settled
   later in the log, never over HTTP. Expiry, eviction, corruption and channel
   removal each go through `settle`/`finish` for this reason.
2. **Transient failures must not delete messages.** A spool file that won't
   *parse* is corrupt and is dropped with a log line; a file that won't *read*
   (EIO, fd exhaustion, permissions) stays queued. The same distinction governs
   config reloads, which retry until they load.

### Config (`internal/config`)

`hubbub.toml` is read once at startup. `keys.toml` and `channels.toml` are
hot-reloaded by `Store.Watch` (2s mtime poll) with validate-before-swap — a
broken hand-edit logs and keeps the previous set rather than blanking
credentials. Successful channel reloads fire `OnChannelsChange`, wired in
`main.go` to `Engine.SetChannels`.

Unknown keys are rejected everywhere (`rejectUnknown`), so a typo fails the load
instead of silently taking a default. A disabled channel is type-checked but not
constructed, so its stale settings can't take the whole file down. Channel ids
are validated as safe single path components because the id names the spool
directory — the engine re-checks this itself (`safeSpoolName`) since a `..`
there is destructive.

## Adding an adapter

Add `internal/adapter/<type>.go`: an `init()` calling `Register("<type>", …)`, a
private config struct with `toml` tags decoded via the `Decode` callback,
validation in the factory, and a `Send` that returns `nil` or one of the three
`SendError` constructors. Fit-to-channel truncation is the adapter's job
(`notify.TruncateBytes`) — ingest caps are deliberately generous. Nothing else
needs touching: `channels.toml` picks it up by `type`.

## Conventions

- Comments here are load-bearing: they record *why* a non-obvious choice was
  made, usually the failure mode it prevents. Preserve them when editing, and
  write in that register rather than narrating what the code does.
- Sanitisation happens once, at ingest (`internal/notify`), and never again.
- Metrics label values are channel ids and outcomes only — never caller ids or
  payload content. The delivery log records a claimed `X-Forwarded-For`, clipped,
  and never trusts it.
- Tests use a scripted `fakeAdapter` and assert on the JSONL delivery log rather
  than on internals; new outbox behaviour should be provable that way too.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/), enforced by the
`commits` job in CI:

```
<type>(<scope>)!: <subject, imperative, lower case, no full stop>
```

`type` is one of `feat` `fix` `docs` `style` `refactor` `perf` `test` `build`
`ci` `chore` `revert`. `scope` is optional and names the area rather than the
file: `config`, `httpapi`, `admin`, `adapter`, `outbox`. Repo-wide changes take
no scope. `deps` is Renovate's, not yours. Subjects stay within 72 characters.

`style` here means presentation only — the served pages' CSS and layout. Code
formatting is not a commit of its own; `gofmt` is a CI gate.

A change to a config file's shape, the request/response contract or a
delivery guarantee is breaking: mark it `!` after the scope and add a
`BREAKING CHANGE:` footer saying what an operator or caller has to do. This
applies before 1.0 too — the README promises config churn, and a promise is
not a reason to leave the churn undocumented.

The body is the point. It records *why*, in the same register as the comments:
the failure mode avoided, the alternative rejected and what it cost, what was
deliberately left alone. A subject that needs no body is usually a commit that
did not need making.
