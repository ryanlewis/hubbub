# CLAUDE.md

Self-hosted notification fan-out hub: one authenticated JSON API in, delivery
to my channels (ntfy first) out. Built to be called by machines — Claude cloud
routines, cron jobs, homelab scripts.

## Source of truth

The reviewed design lives in the Obsidian vault:

- `~/dev/notes/projects/hubbub/design.md` — full design, decision log
  ([R]…[R4] review passes), API contract, outbox semantics
- `~/dev/notes/projects/hubbub/README.md` — overview + v1 roadmap

Read the design doc before changing behaviour; if a change contradicts a
decision-log entry, flag it rather than silently diverging. Design changes get
folded back into the vault doc, not just committed here.

## Hard rules

- **Effectively stdlib.** One dependency, deliberately: `BurntSushi/toml`,
  added 2026-07-25 for config UX (comments in hand-edited operational files).
  It is itself dependency-free. Nothing else goes in without a design-doc
  change first; the `exec` adapter is the extension point, not libraries.
- **Platform-agnostic core.** Anything exe.dev-specific is an adapter/plugin
  (`exe-dev-email`, later `exe-dev-auth`). No tailnet names, no host names,
  no heartbeat provider baked into code — those are config.
- **Flat files only.** TOML (config, keys, channels), JSONL (delivery log),
  maildir-style spool (outbox). No database (SQLite revisit is earmarked in
  the design for `/v1/recent` + `/admin`). Adapters never see the config
  format: the factory is handed an `adapter.Decode` callback.
- Notification content is sanitised once at ingest; adapters truncate
  fit-to-channel; delivery always goes through the outbox (serial worker per
  channel instance).

## Commands

- Build: `go build ./...`
- Test: `go test ./...`
- Vet: `go vet ./...`
- Run locally: `go run . -config example/hubbub.toml`
- Cross-compile for deploy: `GOOS=linux GOARCH=amd64 go build -o hubbub .`

## Deploy target

A dedicated exe.dev VM (`notify`), public mode; scp the binary, systemd unit
restarts it. Ops port sits outside the 3000–9999 proxy range (internet
unreachable). Deployment wiring is runbook, not code.
