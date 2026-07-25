# hubbub

**One authenticated endpoint to notify yourself, fanned out to every channel you care about.**

hubbub is a small self-hosted notification hub. A machine POSTs one JSON
notification; hubbub delivers it to your channels. Callers hold a revocable API
key and nothing else — no ntfy topics, no webhook URLs, no SMTP credentials
scattered across a dozen scripts.

Every send goes through a durable on-disk outbox, so a channel being down means
**late delivery, not lost alerts**.

```sh
curl -X POST https://hub.example.com/v1/notify \
  -H 'Authorization: Bearer nh_...' \
  -d '{"title":"Backup failed","message":"nightly borg run exited 1","priority":"high"}'
```

## Why

Every script and agent that wants to reach you normally has to know about a
specific channel and carry its credentials. Rotating an ntfy topic or a Discord
webhook then means hunting down every caller that embedded it.

hubbub inverts that. Channel credentials live in one place. Callers get their
own named key, and that key's channel list is its permission boundary. Adding a
channel is a config block; revoking a caller is deleting a line.

It is deliberately small: one static binary, flat files, a single dependency. No
database, no runtime to install, no build chain.

## Status

Early. The core vertical works end to end — authenticated notify, the outbox,
the ntfy adapter, the full response contract, metrics, the dead-man heartbeat —
and is covered by tests. Not yet built: the `exec`, email and Discord adapters,
the served OpenAPI spec, bare-URL webhooks, and the admin UI. See
[Roadmap](#roadmap).

Expect breaking changes to config shapes before a tagged release.

## How it works

```mermaid
flowchart LR
    A["Caller<br>cron / script / agent"] -->|"POST /v1/notify<br>Bearer key"| B[hubbub]
    B --> C{{outbox<br>one spool + worker per channel}}
    C --> D[ntfy]
    C -.-> E["exec / email / Discord<br>(planned)"]
```

A request is authenticated, rate-capped, validated, then enqueued into a
per-channel spool. Each channel has exactly one serial delivery worker, so
per-channel ordering is free and shell-out adapters never race themselves.

The HTTP handler waits a short **response window** (default 2.5s) and answers
with whatever is known by then: finished attempts report concretely, anything
still in flight reports `queued`. Retries, backoff and backlogs live *behind*
the response, so a blackholed channel can never hang a caller — worst-case
latency is the window, exactly.

## Quick start

Requires Go 1.26+.

```sh
git clone https://github.com/ryanlewis/hubbub.git
cd hubbub
go build -o hubbub .
```

Point `example/channels.toml` at a real ntfy topic (pick an unguessable one —
on the public ntfy.sh instance the topic name *is* the secret), then:

```sh
./hubbub -config example/hubbub.toml
```

```sh
curl -s localhost:8080/health

curl -s -X POST localhost:8080/v1/notify \
  -H 'Authorization: Bearer nh_dev_key_change_me' \
  -d '{"title":"hello","message":"first message"}'
# {"result":"delivered","requestId":"r_...","channels":{"ntfy":"ok"}}
```

Fire a test send through one channel end to end, from the ops port:

```sh
curl -s -X POST localhost:2112/test/ntfy
```

## Configuration

Three TOML files. All are hot-reloaded on change except `hubbub.toml`, which is
read at startup. Reloads are validate-before-swap: a broken hand-edit logs an
error and keeps the previous configuration rather than blanking your
credentials.

TOML because these are files a human edits under pressure, and being able to
write down *why* a channel is parked or which machine a key belongs to — next
to the thing itself — is worth more than the config format being exotic. A typo'd
key fails the load rather than silently taking its default, and duplicate names
are rejected by the parser.

### `hubbub.toml`

```toml
public_port = 8080
rate_cap_per_hour = 60      # global blast-radius guard; must be >= 1
response_window = "2.5s"
queue_ttl = "6h"
spool_dir = "spool"
spool_cap_per_channel = 100
drain_pace = "2s"
attempt_timeout = "10s"
delivery_log = "delivery.log"
keys_file = "keys.toml"
channels_file = "channels.toml"

# Presence-enabled: delete the table and no ops listener comes up.
[ops]
port = 2112

[heartbeat]
url = "https://hc-ping.com/..."
interval = "60s"
```

| Field | Default | Meaning |
|---|---|---|
| `public_port` | `8080` | Caller-facing API |
| `[ops]` | *(absent)* | Presence-enabled. Brings up a second listener for `/metrics`, `/health` and the test CTA |
| `rate_cap_per_hour` | `60` | Global cap across all keys. Must be ≥ 1 — there is no "unlimited" |
| `response_window` | `2.5s` | How long a request waits on delivery outcomes |
| `queue_ttl` | `6h` | How long an undeliverable message stays queued before it's dropped |
| `spool_cap_per_channel` | `100` | Pending messages per channel; over cap, the oldest is evicted |
| `drain_pace` | `2s` | Gap between sends when draining a backlog |
| `attempt_timeout` | `10s` | Per-attempt timeout handed to the adapter |
| `[heartbeat]` | *(absent)* | Dead-man's-switch ping target. Any provider works. Absent logs a startup warning — it is the only thing that notices a dead process, VM or outbound path |

Durations are strings (`"30s"`, `"2.5s"`, `"6h"`), since TOML has no duration
type. Unknown settings are rejected.

### `keys.toml` — callers and permissions

```toml
[cron]
key = "nh_..."
channels = ["ntfy", "email"]

# Mid-rotation: both keys valid until the caller is flipped over.
[backup-host]
key = ["nh_old...", "nh_new..."]
channels = ["ntfy"]
```

The `channels` list is the caller's **permission boundary and its delivery
set**. A key permitted `["ntfy","email"]` reaches both.

`key` accepts a string or an array, so rotation is add → flip → remove with no
outage window. Both map to the same caller id in the delivery log. Generate keys
from a CSPRNG (`openssl rand -hex 24`); they must be at least 16 characters.

### `channels.toml` — where credentials live

```toml
[ntfy]
type = "ntfy"
server = "https://ntfy.sh"
topic = "unguessable-topic"
token = "tk_..."

# Parked after the topic leaked. Credentials kept; re-enabling is one line.
[standby]
type = "ntfy"
enabled = false
```

The table name is the channel id that permissions and results refer to; `type`
picks the adapter. Several instances of one type are fine. Because the id is a
stable indirection, the technology behind `email` can change without touching
any caller's permission list.

The id also names the channel's spool directory, so it must be a single safe
path component: letters, digits, `-`, `_` and `.`, up to 64 characters, and
never `.` or `..`. Anything else fails the load.

`enabled = false` parks a channel without deleting its credentials, and a parked
block isn't validated — so you can strip a dead channel's settings without
taking the rest of the file down with it.

**Enabling a channel grants no key anything.** New channels are deny-by-default
until a key's permission list names them, so turning on Discord never silently
starts CC-ing it on existing callers.

## API

### `POST /v1/notify`

Authenticate with `Authorization: Bearer <key>`.

```json
{
  "title": "Backup failed",
  "message": "nightly borg run exited 1",
  "priority": "high",
  "tags": ["backup", "storage"],
  "channels": ["ntfy"]
}
```

| Field | Required | Notes |
|---|---|---|
| `title` | yes | Max 256 bytes. All control characters stripped at ingest |
| `message` | yes | Max 4096 bytes. Keeps `\n` and `\t`; other control chars stripped |
| `priority` | no | `low` \| `default` \| `high` \| `urgent`, case-insensitive |
| `tags` | no | Max 16, each max 64 bytes. Control characters stripped |
| `channels` | no | Narrows delivery to a subset of what the key permits |

**Priority is display-only.** It maps to the ntfy priority header and (later) an
email subject prefix. It never changes which channels fire — otherwise a caller
could escalate past its own permission list with `priority: urgent`.

`channels` narrows, never widens, and is not silently intersected: naming a
channel the key lacks is a `403`, so a caller always knows its send didn't do
what it asked. An explicit `"channels": []` is a `400` rather than a
fall-through to the key's full list — a caller that computed no targets must
not fan out to everything. Unknown top-level fields are rejected with `400`, so
a caller learns its schema is stale instead of having a field quietly dropped.

### Response contract

Every outcome is explicit, because the callers are machines that must be able to
tell delivered from dropped-silently.

| Status | `result` | Meaning |
|---|---|---|
| `200` | `delivered` | Every selected channel delivered inside the window |
| `202` | `queued` | Accepted, nothing delivered yet. A **promise, not a receipt** |
| `207` | `partial` | Mixed: some channels `ok`, others `queued`, `failed` or `disabled` |
| `502` | `failed` | Every selected channel failed permanently |
| `429` | `rate_capped` | Global cap hit; carries `Retry-After` |
| `400` / `401` / `403` | — | Malformed · bad credential · channel not permitted |

```json
{
  "result": "partial",
  "requestId": "r_8f3ka2...",
  "channels": { "ntfy": "ok", "email": "queued" }
}
```

Branch on the status code first, `result` second. Per-channel values are `ok`,
`queued`, `disabled`, `failed: <reason>`, or `dropped: <reason>`. `requestId`
matches the delivery log, so a caller-side error report can be tied to hubbub's
own record of what happened.

A `disabled` channel was never attempted, so it is not a permanent failure: a
selection that is entirely disabled answers `207`, not `502`. That keeps it a
visible config nag instead of inviting generic 5xx retry machinery to hammer a
request that cannot succeed until `channels.toml` is edited.

A `202` is settled later in the delivery log, not over HTTP — a queued message
that eventually expires shows up there and in `/metrics`.

### Ops endpoints

Served on the ops port, which is intended to sit somewhere the internet can't
reach. That placement *is* the access control — these carry no auth of their own.

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness: 200 with version and uptime. Also on the public port |
| `GET /metrics` | Prometheus counters |
| `POST /test/{channel}` | Fire a canned notification through one adapter, jumping the queue |

## Delivery guarantees

- **At-least-once.** A caller that times out and retries may double-send. The
  cost of a duplicate is a second phone buzz; the cost of a lost alert is worse.
- **Nothing is dropped without a log line.** Expiry, eviction, corruption and
  channel removal each write a terminal record with the request id.
- **A channel outage is late delivery, not loss.** Messages queue through the
  outage, bounded by `queueTTL` and `spoolCapPerChannel` (evict-oldest — fresh
  news beats stale news), and drain paced and oldest-first on recovery.
- **Retries stay on the same channel.** Timeouts, network errors and `5xx` are
  retried with backoff; `4xx` never is. A `429` re-queues honouring `Retry-After`
  as a not-before, floored at 30s. There is no cross-channel fallback — that
  would bypass the permission boundary.
- **Adapters fit the message to the channel**, truncating with a marker rather
  than letting the channel reject an over-length body.

## Operating it

**Metrics** (`/metrics`) are monotonic counters, so each consumer computes its
own windows:

```
notify_requests_total{outcome="delivered"}
notify_deliveries_total{channel="ntfy",outcome="ok"}
notify_auth_failures_total
process_start_time_seconds
```

Label values are channel ids and outcomes only — never caller ids or payload
content.

**The delivery log** is JSONL, one line per notification at accept time plus a
terminal line for anything that settles later, and separate lines for auth
failures and rate caps. It is append-only and greppable; give it a `logrotate`
unit, since an unbounded log filling a small disk is itself a silent failure.

**The dead-man's switch**, if configured, pings an external service on a timer —
and each tick is gated on hubbub probing its own public listener. A bare timer
attests only that the process is alive; the self-probe makes the ping attest
that it is *serving*, so a wedged listener trips the switch instead of staying
invisibly green. Process death, host death and lost outbound connectivity all
stop the pings by construction.

Channel-level failure is deliberately *not* the dead-man's job — one broken
channel shouldn't declare the whole hub dead. That's what `/metrics` and the
test CTA are for.

**The habit worth keeping:** end every `channels.toml` edit by firing
`POST /test/{channel}` and watching the notification arrive. It's the only check
that catches a typo'd topic delivering cheerfully into a stranger's channel, or
a push that the upstream accepted but your phone never showed.

## Deployment

One static binary and three files. Cross-compile on your laptop and copy it up —
pure stdlib means no CGO:

```sh
GOOS=linux GOARCH=amd64 go build -o hubbub .
```

Checklist for a real deployment:

- A supervisor (systemd unit) with restart-on-failure and start-on-boot
- `logrotate` for the delivery log
- The ops port bound somewhere the internet cannot reach
- A dead-man's-switch ping target configured
- An **encrypted off-host backup of `keys.toml` and `channels.toml`** — they are
  the only state that hurts to lose, and losing them means re-issuing every
  caller's key and re-finding every webhook URL
- A first test-CTA fire against every enabled channel

Alerting built on hubbub's metrics must not route *through* hubbub. That
circular dependency is how a notification system fails silently.

## Development

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
```

Two rules the codebase holds to:

1. **Effectively stdlib.** The only dependency is `BurntSushi/toml`, which is
   itself dependency-free, so the whole module graph is two entries. Anything
   further needs a real justification: the planned `exec` adapter is the
   extension point for new behaviour, not third-party libraries.
2. **Platform-agnostic core.** Anything tied to a specific host or provider is an
   adapter or a config value, never baked into the core.

## Roadmap

- [x] `POST /v1/notify`, ntfy adapter, per-key channel permissions, global rate cap
- [x] Outbox delivery engine — spool, serial workers, wait-window responses
- [x] JSONL delivery log, `/health`, `/metrics` + ops port, test-send CTA
- [x] Self-probing dead-man's-switch heartbeat
- [ ] `GET /openapi.json` — served spec so an agent can be pointed at the base URL
- [ ] `exec` adapter — shell-out channels, no rebuild required
- [ ] Email and Discord adapters
- [ ] Per-key rate caps and idempotency keys
- [ ] Bare-URL webhooks (`POST /hook/<token>`) for senders that can't set headers
- [ ] `GET /v1/recent` and an `/admin` dashboard

## Prior art

[Apprise](https://github.com/caronc/apprise-api) is the closest off-the-shelf
option and fans out to far more services, but has no authentication by design —
you bolt it on with a reverse proxy, with no per-key issue and revoke.
[ntfy](https://ntfy.sh/) is a delivery target rather than a fan-out hub, and is
hubbub's first adapter. [Novu](https://novu.co/) is real notification
infrastructure and considerably more than a personal hub needs.
