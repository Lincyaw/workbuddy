# Session Data Ownership: Worker Owns, Coordinator Proxies

- Date: 2026-05-01
- Status: accepted (Phase 1 implemented)
- Tracks: REQ-120

## Context

In v0.5 workbuddy split into a 3-unit systemd bundle: `supervisor + coordinator + worker`. The coordinator and worker each open their own SQLite file:

- coordinator → `.workbuddy/workbuddy.db`
- worker     → `.workbuddy/worker.db`

Sessions are produced on the worker side. `internal/runtime/session_manager.go` writes session rows into `worker.db` because it runs in the worker process, alongside the per-session artefacts on disk (`.workbuddy/sessions/<id>/{events-v1.jsonl,stdout,stderr,metadata.json}`).

The coordinator-side audit API in `internal/auditapi/handler.go` reads from `workbuddy.db`. There is **no replication channel** between the two stores. Consequence: every session created since the v0.5 cutover is invisible to the coordinator API. `listDiskOnlySessions` (handler.go:1644) and `buildDegradedFromDisk` (handler.go:1733) fall back to disk-walk synthesis and surface every healthy session as `aborted_before_start` in the dashboard. Operators see red warning cards for runs that completed normally.

We considered four options:

1. **Replicate session rows worker → coordinator over HTTP.** Cheapest in lines of code, but every replication path is a future bug — eventual consistency, idempotency on retry, schema drift between stores, and a new failure mode where a worker that has lost its coordinator connection still produces sessions but they're invisible until the link recovers. Also doubles SQLite write traffic.
2. **Move the session DB to a shared filesystem.** Loses split-host topology — a worker on a private subnet can no longer be paired with a coordinator on the public internet without exposing its DB.
3. **Put session writes in the coordinator and have workers stream events upstream.** Reverses the locality: today the worker owns the disk artefacts the DB rows describe. Splitting them invites the same drift `auditapi` already trips on, just inverted.
4. **Worker owns session data; coordinator becomes a proxy.** The worker is already the writer. Make it the reader too. Coordinator forwards session-shaped requests to the owning worker via a registered `audit_url`.

We chose option 4. Locality matches authority — the process that produces the data is the process that exposes it.

## Decision

The refactor ships in three phases. Only Phase 1 is implemented today.

### Phase 1 — additive (this PR, REQ-120)

1. **New worker-side audit HTTP server** (`internal/worker/audit/`).
   - Endpoints: `GET /api/v1/sessions[?repo=&agent=&issue=&limit=&offset=]`, `/sessions/{id}`, `/sessions/{id}/events`, `/sessions/{id}/stream` (SSE), `/health`.
   - Reads from the worker's own `*store.Store` and `sessionsDir`.
   - Reuses `internal/auditapi.Handler` with a new `SetDisableDiskOnlySynthesis(true)` knob: a missing DB row is a real `404`, not a synthesized `aborted_before_start`. The `degraded:no_events_file` flag for terminal-status rows missing their events log is preserved (it's a real signal, just one the worker can answer truthfully).
   - Reuses `internal/webui.Handler` unchanged for events/stream tailing.
2. **`workbuddy worker --audit-listen`** flag (`cmd/worker.go`). Default `127.0.0.1:0`. Special values `""` and `disabled` opt out (the register payload then omits `audit_url`). `0.0.0.0:N` exposes on all interfaces; the resolved address is logged.
3. **Bearer-token auth.** The audit listener requires `Authorization: Bearer <token>`, same `WORKBUDDY_AUTH_TOKEN` the worker uses outbound. Constant-time compare. **Empty token = fail-closed startup**: the audit server refuses to start, so an operator cannot accidentally expose unauthenticated session reads. Disable explicitly via `--audit-listen=disabled`.
4. **Register-protocol extension.** `workerclient.RegisterRequest` gains `AuditURL string \`json:"audit_url,omitempty"\``. The worker computes its advertised URL: `--audit-public-url` wins; otherwise `http://<bind-host>:<bind-port>` derived from the resolved listener; `0.0.0.0`/`::` bind hosts are substituted with the worker hostname so coordinator-side proxying in Phase 2 has a reachable URL.
5. **Coordinator persistence.** `internal/app/coordinator.go` reads the new field, `internal/registry.RegisterWithRepos` accepts it, `internal/store` migrates the `workers` table with `ALTER TABLE workers ADD COLUMN audit_url TEXT NOT NULL DEFAULT ''`. Existing rows survive with empty values; the coordinator's read paths are otherwise unchanged. **Phase 1 stores the URL but does not use it.**

### Phase 2 — coordinator becomes a proxy (NOT in this PR)

- Coordinator-side `/api/v1/sessions[/{id}/events|/stream]` is rewritten to look up the owning worker by `(repo, session_id)` and reverse-proxy the request to that worker's `audit_url + path`, forwarding the bearer token.
- For list endpoints (no session_id), fan out to all online workers bound to the requested repo (or all workers when repo filter is empty), merge results, sort by `created_at`. Per-worker errors degrade to a `degraded:worker_unreachable` row rather than a 500.
- `coordinator_session_proxy.go` (which today proxies to `mgmt_base_url`) consolidates with the new path or is deprecated; whichever produces less code.
- Web UI is unchanged — it still hits coordinator endpoints. Only the implementation moves.

### Phase 3 — delete legacy paths (NOT in this PR)

- Remove `listDiskOnlySessions`, `buildDegradedFromDisk`, `StatusAbortedBeforeStart`, `degradedReasonNoDBRow`, the disk-walk metadata reader, and the synthesis tests once Phase 2 has been live long enough for the operator playbooks to retire.
- The coordinator DB no longer holds session rows; we can drop the `sessions` table on the coordinator side (the `agent_sessions` legacy table goes too, if no other reader survives). Migration deferred to its own ADR.

## Phase 1 boundaries that Phase 2 must respect

- **Coordinator does NOT read `audit_url` yet.** It writes it and that's it. Phase 2's seam is the coordinator's session list/detail handler in `internal/auditapi/handler.go` — that's where the proxy logic lands.
- **Worker handler intentionally exposes only the session subset of `/api/v1`.** It does not implement `/api/v1/issues/...`, `/api/v1/workers`, `/api/v1/metrics`, `/api/v1/alerts` — the worker DB doesn't carry that state. If Phase 2 wants worker-side anything else, that's a new design call.
- **No deletion in Phase 1.** `auditapi.Handler` keeps every legacy code path. The new `SetDisableDiskOnlySynthesis` knob is opt-in; coordinator-side handlers leave it false so the existing degraded-fallback behavior is byte-identical to v0.5.

## Why fail-closed on empty token

The audit listener serves session content (stdout, stderr, conversation events). If a deployment forgets to set `WORKBUDDY_AUTH_TOKEN`, "fail open with no auth" silently exposes that content to anyone who can reach the bind address. Forcing the operator to type `--audit-listen=disabled` is one extra character against a much larger blast radius. The systemd unit always sets the env var (it's the same one used for outbound auth), so production deployments are unaffected.

## Why expose the session subset, not the full dashboard API

The full `/api/v1/*` shape on the worker would imply the worker has issue caches, alerts, metrics, transitions, etc. — it does not. Mounting those routes pointing at an empty store would either return empty arrays (silently misleading) or 500s. The narrower surface forces clients (Phase 2 coordinator, external tooling) to ask the right server for the right data.
