# Coordinator Hook System

- Date: 2026-04-30
- Status: accepted
- Issue: #262

## Context

`internal/notifier/notifier.go` ships a hardcoded notifier that subscribes to alertbus + task completion and sends Slack/Feishu/Telegram/SMTP. It's rigid:

- Only two trigger points (alert + task done). `state_entry`, `dispatch`, `dev_review_cycle_cap_reached`, `dispatch_blocked_by_dependency`, `coordinator_restart_gap` are invisible.
- Channel set is compiled in. New destinations (DingTalk, PagerDuty, custom webhook, local script) require Go changes.
- No declarative routing (per-repo / per-severity / per-label).

We want users to declaratively describe *event → action* and have the coordinator execute it asynchronously.

## Decision

### Configuration is host-global, not per-repo

Hooks live in `~/.config/workbuddy/hooks.yaml` (single file, same directory as `worker.env`). CLI `--hooks-config` overrides the path.

`.github/workbuddy/hooks/*.md` is intentionally **not** supported. Rationale: per-repo hooks would let any PR contributor inject a `command:` action and gain RCE on the coordinator host. By restricting hooks to the operator's filesystem, the trust boundary collapses to "whoever has write access to `/home/<user>/.config/workbuddy/`" — same as the database, the systemd unit, and worker tokens.

A consequence: we don't need an opt-in flag for `command` actions. The earlier draft (`--hooks-allow-command`) is gone.

### One hook point: `eventlog.Logger.Log()`

The dispatcher hooks into `internal/eventlog/eventlog.go` `Log(...)` after the SQLite insert. **Every existing event type is automatically subscribable**. Adding a new subscribable event = adding a new `eventlog.Log(...)` call in the originating code path. No new hook points.

This is the load-bearing decision: hook surface area = eventlog type set. Documentation maintains one list.

Three places currently log via `log.Printf` instead of `eventlog.Log` will be retrofitted to emit eventlog entries (Phase 2):

- `coordinator_started` (cmd/serve.go, cmd/coordinator.go)
- `coordinator_stopping` (graceful shutdown handler)
- `config_reloaded` (auditapi config reload handler)

### Action types (both default-on)

| Type | Behaviour |
|---|---|
| `webhook` | POST v1 payload as application/json. 2xx = success. Headers support `${ENV_VAR}` substitution resolved at startup. |
| `command` | Execute argv (no shell expansion). v1 payload on stdin. Env adds `WORKBUDDY_EVENT_TYPE`, `WORKBUDDY_REPO`, `WORKBUDDY_ISSUE_NUM`, `WORKBUDDY_HOOK_NAME`. Non-zero exit = failure. |

Action types are pluggable via `internal/hooks/action.go` `ActionRegistry`. Adding a new type (e.g. SMTP, gRPC, pubsub) is a Go change but does not touch the dispatcher.

### Dispatcher behaviour

- **Async, buffered**: 1024-deep channel + worker pool. Default 4 workers. Overflow drops the message and increments `workbuddy_hook_overflow_total`. State machine is never blocked by hook latency.
- **Per-hook independence**: each hook has its own bounded execution slot so a slow hook can't starve others.
- **Timeout**: default 5s, configurable per hook. Webhook → request cancellation. Command → SIGTERM, then SIGKILL after a 2s grace (matches existing agent cancel semantics in `internal/supervisor/agent.go`).
- **Auto-disable**: 5 consecutive failures disable the hook in-memory and emit `hook_disabled`. **No half-open probing** — operator must `workbuddy hooks reload`. Rationale: hooks are user-owned scripts/endpoints; if they're broken, eyes-on is the right response, not silent recovery.
- **Self-amplification guard**: events with type prefix `hook_` (`hook_disabled`, `hook_failed`, `hook_overflow`) are written to eventlog but skipped by the dispatcher. Without this guard, a failing hook on `*` would log a `hook_failed`, which would re-trigger itself.

### Stable v1 event payload

The dispatcher does not pipe eventlog's internal JSON straight to user actions. `internal/hooks/eventpayload.go` translates each event into a versioned envelope:

```json
{
  "schema_version": 1,
  "event_type": "state_entry",
  "ts": "2026-04-30T03:23:27Z",
  "repo": "Lincyaw/workbuddy",
  "issue_num": 250,
  "data": { /* event-specific payload */ }
}
```

`docs/hooks.md` lists every field; the `data` shape per event_type is also enumerated. **Fields are append-only**. Removing or renaming a field requires bumping `schema_version`.

### Coexistence with `internal/notifier`

Both run in parallel during the rollout. The notifier is not modified or removed by issue #262. A future issue (out of scope here) will:

1. Write a migration guide showing the same Slack/Feishu/Telegram/SMTP behaviour in hook config.
2. Reimplement the built-in notifier as auto-registered internal hooks.
3. Delete `internal/notifier/notifier.go` once the migration sees real-world use.

### CLI surface

- `workbuddy hooks list` — registered hooks
- `workbuddy hooks test --hook NAME --event-fixture path/to/event.json` — fire once with a fixture; **does not** write to eventlog
- `workbuddy hooks status` — recent invocations, success/failure rate, auto-disable state
- `workbuddy hooks reload` — re-read config and reset `hook_disabled` flags

### Phase plan

| Phase | Scope |
|---|---|
| 1a | `internal/hooks/` package skeleton, YAML parser, dispatcher, `ActionRegistry`, webhook action, eventlog hook, `workbuddy hooks list` |
| 1b | command action, timeout/SIGKILL, auto-disable, self-amplification filter, `workbuddy hooks test` |
| 2 | `match.severity` / `match.repo` filters, metrics, `workbuddy hooks status` / `hooks reload`, eventlog entries for `coordinator_started` / `_stopping` / `config_reloaded` |
| 3 | webui Hooks page (list + recent 20 invocations + error rate) |

Each phase is independently mergeable. `depends_on:` enforces serial execution.

## Consequences

### Positive

- Hook surface is one file in code (`eventlog.Log`) and one config file on disk. Trivial mental model.
- Adding subscribable events = `eventlog.Log(...)`. No "extend the hook system" recurring task.
- No PR-injected RCE. Operator-only trust.
- v1 payload is decoupled from internal eventlog JSON, giving us room to refactor eventlog without breaking user scripts.

### Negative

- Per-repo hook customization is impossible by design. Workaround: hook author filters on `payload.repo` inside their script/endpoint.
- The `internal/notifier` migration is deferred. We carry two notification paths until that future issue lands.
- `command` action runs as the coordinator process uid. Operator must vet scripts; we don't sandbox.

## Alternatives considered

- **Per-repo hooks at `.github/workbuddy/hooks/*.md`**: matches the existing agent/workflow config style but introduces a PR-injected RCE vector. Rejected.
- **Plugin system (`.so` / RPC)**: more flexible than command/webhook but adds significant build-time, deployment, and security complexity. command + webhook covers >99% of expected use. Rejected.
- **Reuse `internal/notifier` as the hook engine**: would couple the new declarative system to the legacy hardcoded one and complicate the eventual notifier removal. Rejected; kept them parallel instead.
- **Replay-style retries with circuit breaker**: half-open probing adds operational nuance for marginal benefit. Rejected in favour of explicit operator reload.
