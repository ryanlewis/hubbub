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
the ntfy and smtp adapters, the full response contract, metrics, the dead-man
heartbeat, the served OpenAPI spec and the `/admin` dashboard — and is covered
by tests. Not yet built: the `exec` and Discord adapters, bare-URL webhooks,
per-key rate caps and `/v1/recent`. See [Roadmap](#roadmap).

Expect breaking changes to config shapes before a tagged release.

## How it works

```mermaid
flowchart LR
    A["Caller<br>cron / script / agent"] -->|"POST /v1/notify<br>Bearer key"| B[hubbub]
    B --> C{{outbox<br>one spool + worker per channel}}
    C --> D[ntfy]
    C --> E["smtp<br>email, HTML + text"]
    C -.-> F["exec / Discord<br>(planned)"]
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

The API describes itself, including the channel ids this instance has:

```sh
curl -s localhost:8080/openapi.json
```

A browser pointed at `http://localhost:8080/` gets a short human-readable
version of the same contract, `/docs` gets a browsable reference you can fire
requests from, and `/llms.txt` gets it condensed for an agent.

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

# Presence-enabled: delete the table and /admin is never registered.
[admin]
auth = "exe-dev"
allowed_emails = ["you@example.com"]
```

| Field | Default | Meaning |
|---|---|---|
| `public_port` | `8080` | Caller-facing API |
| `[ops]` | *(absent)* | Presence-enabled. Brings up a second listener for `/metrics`, `/health` and the test CTA |
| `[admin]` | *(absent)* | Presence-enabled. Serves the [dashboard](#the-dashboard) on the public listener. `allowed_emails` must name at least one address — there is no "allow everyone" |
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

[email]
type = "smtp"
host = "smtp.mail.me.com"
port = 587
username = "you@icloud.com"
password = "app-specific-password"
from = "you@icloud.com"
from_name = "hubbub"
to = ["you@icloud.com"]

# Parked after the topic leaked. Credentials kept; re-enabling is one line.
[standby]
type = "ntfy"
enabled = false
```

#### Adapter types

| `type` | Settings |
|---|---|
| `ntfy` | `server` (default `https://ntfy.sh`), `topic` (required), `token` |
| `smtp` | `host` (required), `port`, `username`, `password`, `from` (required), `from_name`, `to` (required, a list), `tls`, `helo`, `rate_limit_cooldown` |

`smtp` is generic submission — iCloud, Fastmail, Gmail or a relay on localhost
are all just a `host`. It sends `multipart/alternative`: the caller's `html` (or
a mail hubbub composes from the notification) alongside `message` as the plain
part.

`tls` is `starttls` (default), `implicit` or `none`, and defaults from the port —
`465` means implicit, anything else means STARTTLS. A server that stops
advertising STARTTLS is refused rather than downgraded, because the alternative
is putting the password on the wire. `tls = "none"` with a `password` set fails
the config load: Go's SMTP client won't transmit credentials over a bare
connection anyway, so the channel would look configured and fail every send.

`rate_limit_cooldown` (default `15m`) is how long to park the channel after a
`4xx` that names a send limit. SMTP has no `Retry-After`, and the generic retry
backoff tops out at five minutes — which against a provider's daily quota is
just a busier way to keep being refused.

For iCloud specifically: `password` must be an [app-specific
password](https://support.apple.com/en-us/102654), `from` must be your iCloud
address or a verified alias, and if auth fails with credentials you know are
right, try `username` as the part before the `@`.

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
| `html` | no | Max 128 KB. Rich body for channels that render one; see below |
| `priority` | no | `low` \| `default` \| `high` \| `urgent`, case-insensitive |
| `tags` | no | Max 16, each max 64 bytes. Control characters stripped |
| `channels` | no | Narrows delivery to a subset of what the key permits |

**Priority is display-only.** It maps to the ntfy priority header and to an
email subject prefix (`[HIGH]`, `[URGENT]`) plus `Importance`. It never changes
which channels fire — otherwise a caller could escalate past its own permission
list with `priority: urgent`.

**`html` lets the producer own the body.** Channels that can render it do
(`smtp`); text-only channels ignore it entirely and show `message`, which is why
`message` stays required — it is the `text/plain` alternative in the mail, and
the only thing a push channel has to work with.

A fragment is wrapped in hubbub's styled shell, so `"<p>done</p>"` arrives as a
complete email. Anything starting `<!doctype` or `<html` is treated as a whole
document and used verbatim — wrapping it would nest `<html>` inside a `<body>`
and leave the result to each client's error recovery.

It is passed through as written. Control characters are stripped; no tags,
attributes or URLs are filtered. That is a deliberate trust decision: posting at
all requires a key the operator issued to a machine the operator runs, which is
the same trust that already lets a caller put arbitrary *text* in front of the
operator, and the receiving mail client is itself a sanitiser that drops scripts
and blocks remote images. Sanitising properly would mean parsing HTML, which the
Go standard library cannot do — and a parser is not worth a dependency for
content that is already trusted. **Treat "can post to this hub" as "can put
arbitrary markup in the operator's inbox"** when deciding who gets a key.

`channels` narrows, never widens, and is not silently intersected: naming a
channel the key lacks is a `403`, so a caller always knows its send didn't do
what it asked. An explicit `"channels": []` — or `null` — is a `400` rather than
a fall-through to the key's full list: a caller that computed no targets must
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

### `GET /openapi.json`

The served OpenAPI 3.1 spec, on the public port and deliberately **unauthenticated**:
it describes the shape of the API, which is not a secret, so an agent can be
pointed at the base URL and discover the contract before it holds a key.

Three parts are filled in per request rather than checked into the file, because
each is a property of the deployment rather than of the API:

| Injected | Why it can't be hardcoded |
|---|---|
| `channels` enum | Channel ids are whatever the operator configured. Includes disabled ones — naming one is a `disabled` result, not an error |
| `servers[0].url` | Reconstructed from `X-Forwarded-Proto`/`-Host`, so a TLS-terminating proxy in front doesn't produce an `http://` base for an HTTPS-only hub |
| `info.version` | Belongs to the binary, so a deployment can be identified from outside |

The spec is only worth serving if it's true, so it is asserted against the real
handler rather than reviewed by eye: `openapi_test.go` replays every example
request through the live mux, and pins the documented field set, priority enum
and status codes to what the code actually does. Adding a request field without
documenting it fails the suite.

### `GET /docs`

The browsable API reference — endpoint list, request and response schemas with
their constraints, every documented status, and a panel that fires a real
request against this hub.

It is **rendered from the spec on every load**, never maintained beside it. The
page can't document an endpoint the spec lacks or omit one it has, and the
field table is generated from the schemas rather than typed out, so a new field
appears there the moment it appears in the contract.

It is deliberately not Swagger UI, Redoc or Scalar. Those are 0.8–3.6 MB of
vendored minified JavaScript to render a five-endpoint API, against a design
whose one sanctioned browser asset is ~14 kB of HTMX — and a docs page that
takes a bearer key is the last place to want a third-party bundle, CDN-hosted or
not. Server-rendering with `html/template`, already linked in for the landing
page, adds nothing to the binary and keeps the module graph at two entries.

The **Send** button is real: on a notification hub, "try it out" means an actual
push, a line in the delivery log and one of the hourly rate cap. The panel says
so. The key is typed per session into a password field, kept in the tab, never
written to `localStorage` or a cookie, and sent to nothing but this hub — the
request is same-origin and relative, so it can't follow the spec's `servers` URL
somewhere else.

This is the one page in hubbub where a credential is entered, so it is the one
that carries a `Content-Security-Policy`: `default-src 'none'` with a
per-request nonce for its own inline script and style, and `connect-src 'self'`.
A test asserts the nonce changes between requests — a fixed one would be no
better than `'unsafe-inline'`.

Unlike `/` and `/llms.txt`, this page *does* reflect the deployment: it renders
the spec, so it shows the configured channel ids, exactly as `/openapi.json`
already does.

### `GET /` and `GET /llms.txt`

Two unauthenticated descriptions of the same contract, for the readers who
arrive without a key: a person who opened the base URL in a browser, and an
agent handed the host and nothing else. `/` is a small self-contained HTML page
(also served at `/index.html`); `/llms.txt` follows the
[llms.txt convention](https://llmstxt.org) — markdown, served as `text/plain`
so it renders wherever it's fetched rather than prompting a download.

Both name the host they were reached on, so the `curl` example on the page is
copy-pasteable as-is, and both are pinned by tests to the status codes the spec
documents. The response contract is now written down in three places, and the
copy nobody re-reads is the one that rots.

**They describe the API, never the deployment behind it.** No channel ids, no
caller ids, no ops endpoints — the two handlers are the only ones in the package
that don't take the server, which is the cheapest way to guarantee they have
nothing to leak. `/openapi.json` is the endpoint that *does* reflect this
particular hub, and the one to point an agent at.

The page carries `noindex`: a private hub gains nothing from being in a search
index. Delete the meta tag if yours should be findable.

### Ops endpoints

Served on the ops port, which is intended to sit somewhere the internet can't
reach. That placement *is* the access control — these carry no auth of their own.

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness: 200 with version and uptime. Also on the public port |
| `GET /metrics` | Prometheus counters |
| `POST /test/{channel}` | Fire a canned notification through one adapter, jumping the queue |

## The dashboard

`GET /admin` edits the two things that actually change over a hub's life —
which channels are on, and which keys may use them — in a browser instead of
over SSH. It is off unless `[admin]` is configured.

It can create a caller and mint its key, edit channel grants, rotate and revoke
keys, add/enable/disable/delete a channel, edit a channel's settings, and fire
the test send. Everything it writes goes into the same `keys.toml` and
`channels.toml` you edit by hand.

Each half of the page names the other. A channel row lists the callers that may
send to it, which is the blast radius of the Delete button beside it, and a
caller's grants mark any channel that is currently paused — a grant on a
disabled channel is live config that delivers nothing. The last key a caller
holds cannot be revoked from the page: a caller with no key fails the load and
takes every other caller down with it, so deleting the caller is the operation
on offer instead.

**Authentication is an identity provider, not a login hubbub rolled itself.**
`auth = "exe-dev"` reads the `X-ExeDev-Email` header that exe.dev's proxy
injects, and bounces anonymous browsers through `/__exe.dev/login`. Only the
addresses in `allowed_emails` get in; anyone else gets a 403 that names the
address it refused, because being signed in as the wrong one of your own
accounts is the usual cause.

> **The `exe-dev` provider is only safe behind the exe.dev proxy.** It trusts a
> header, and that trust holds because the proxy strips any copy a client
> sends. Reach the port some other way — a local `go run`, a port exposed
> around the proxy — and the header is whatever the caller says it is, so the
> dashboard is whatever the caller says it is too. hubbub logs this warning at
> every start rather than leaving it to be remembered.

`/`, `/docs` and `/admin` share a navigation bar, and the dashboard link on it
is decided per visitor: it appears for an address on `allowed_emails` and for
nobody else. The first two pages are readable by anyone who can reach the hub,
so a link everyone can see would be a dead end for them and a signpost for
everyone else.

Notes on how it treats your files, since they hold every credential you have:

- **Comments survive.** Edits are line-level splices, not a decode/re-encode
  round trip, so the annotations you left next to a parked channel stay where
  they are. Untouched regions come back byte-identical.
- **It cannot write a file hubbub then fails to load.** Candidates are checked
  by the real loader before anything lands; a rejected edit shows the loader's
  own error and leaves the file alone.
- **A concurrent hand-edit wins.** Each page carries a content hash; if the
  file moved underneath you the save is a `409` and your change is not applied.
- **Destructive and freeform edits show a diff first.** Nothing structural can
  detect a lost comment — a comment has no semantics — so you get to see
  `−40 +2` before you agree to it. The diff is masked as well: a bearer key
  shows as its prefix and a channel credential as `••••••••`, on the unchanged
  lines your edit sits between as much as on the changed ones.
- **A key is shown once, when it is created.** After that only a prefix. Rotate
  rather than recover.
- **Credentials render masked** in the settings editor; leave the mask alone to
  keep the value on disk. Nothing already on disk is sent to the browser; a new
  value you type comes back once, in the confirmation form that applies it.
- **Every change is logged** to the delivery log as `kind: "admin"`, with the
  address that made it, and counted in `notify_admin_changes_total`.

Two things it deliberately will not do: rename a channel (that reads to the
outbox as *channel removed*, which settles the backlog and deletes the spool),
and edit `hubbub.toml` (read once at startup, so an edit would silently not
apply until a restart).

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

Rotate it with **`copytruncate`**. hubbub opens the log once at startup and holds
that descriptor, so the default rename-and-create rotation leaves it writing into
the rotated-away file: the new `delivery.log` stays empty while terminal
outcomes disappear into a file that the next rotation compresses or deletes.
`copytruncate` keeps the descriptor valid.

```
/var/lib/hubbub/delivery.log {
    weekly
    rotate 8
    compress
    copytruncate
    missingok
    notifempty
}
```

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
- If `[admin]` is enabled, a config **directory** the service may write to. The
  dashboard saves by writing a temp file next to the real one and renaming, so
  write permission on the files alone is not enough. Sandboxing is the usual
  thing in the way: under systemd's `ProtectSystem=strict` every save fails with
  *read-only file system* until the directory is named in `ReadWritePaths=`
- `logrotate` for the delivery log, with `copytruncate` — the log descriptor is
  held open, so rename-and-create rotation silently orphans every later line
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
- [x] `GET /openapi.json` — served spec so an agent can be pointed at the base
      URL, plus `/`, `/docs` and `/llms.txt` for readers who arrive without a key
- [x] `smtp` adapter — email as `multipart/alternative`, with an optional
      caller-supplied `html` body
- [ ] `exec` adapter — shell-out channels, no rebuild required
- [ ] Discord adapter
- [ ] Per-key rate caps and idempotency keys
- [ ] Bare-URL webhooks (`POST /hook/<token>`) for senders that can't set headers
- [x] `/admin` dashboard — keys, grants and channel settings in a browser,
      behind a pluggable identity provider (`exe-dev` first)
- [ ] `GET /v1/recent` — a delivery-log view

## Prior art

[Apprise](https://github.com/caronc/apprise-api) is the closest off-the-shelf
option and fans out to far more services, but has no authentication by design —
you bolt it on with a reverse proxy, with no per-key issue and revoke.
[ntfy](https://ntfy.sh/) is a delivery target rather than a fan-out hub, and is
hubbub's first adapter. [Novu](https://novu.co/) is real notification
infrastructure and considerably more than a personal hub needs.
