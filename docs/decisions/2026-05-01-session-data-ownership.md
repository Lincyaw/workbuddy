# Session Data Ownership: Worker Owns, Coordinator Proxies

- Date: 2026-05-01
- Status: accepted (Phase 1 + Phase 2 implemented)
- Tracks: REQ-120, REQ-121

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

### Phase 2 — coordinator becomes a proxy (REQ-121, implemented 2026-05-01)

- New `internal/sessionproxy` package owns `/api/v1/sessions` (fan-out)
  and `/api/v1/sessions/{id}[/events|/stream]` (single-session proxy).
- `Resolver` chains `session_id → worker_id → audit_url` via the
  coordinator's local sessions + workers tables (Phase 1 persisted the
  audit_url). Sentinel errors `ErrSessionNotRouted`,
  `ErrAuditURLMissing`, `ErrWorkerOffline` translate to 404 / 503 and
  drive the local-fallback decision.
- Single-session detail proxies the request to
  `<audit_url>/api/v1/sessions/{id}[suffix]?<query>` and forwards the
  coordinator's `WORKBUDDY_AUTH_TOKEN` as `Authorization: Bearer …`.
  Status + body pass through; we copy `Content-Type` only — worker
  cookies / arbitrary headers do not leak to the browser.
- SSE pass-through pumps bytes through with per-chunk
  `http.Flusher.Flush()`, never decoding/re-encoding. `r.Context()`
  cancellation propagates to the upstream request, so a browser
  disconnect closes both legs.
- Fan-out listing: candidate workers come from `Store.QueryWorkers(repo)`
  (which already filters by `workers.repos_json` / legacy `repo`). Each
  worker is hit concurrently with a 3 s per-call timeout and a 6 s
  overall budget. Slow / offline workers do not block the response;
  their IDs go into the `X-Workbuddy-Worker-Offline` response header
  (and a reserved envelope field for a future opt-in shape upgrade).
  Results are deduped by `session_id`, sorted by `created_at` desc, and
  paginated against the request's `limit`/`offset`.
- A 5 s in-memory cache fronts the listing keyed on
  `(repo, agent, issue, limit, offset)`. Cached only when no upstream
  errors occurred — a partially-degraded response should not pin
  itself for 5 s. `Handler.InvalidateCache()` is exported so a future
  hook can drop on `worker_registered` events.
- Local fallback: when `Resolve` returns a session whose row carries no
  worker, or whose worker no longer exists, the request falls through
  to the existing in-process audit handler so legacy pre-bundle
  sessions on the coordinator host stay reachable. The optional
  loopback short-circuit (audit_url points at 127.0.0.1 / the
  coordinator's hostname) is opt-in via `WithLocalAuditFallback(true)`
  — a worker bound on loopback is already a Phase-1 misconfiguration,
  not something to compensate for in routing.
- Coordinator-side `dashboardAPI.SetDisableDiskOnlySynthesis(true)` is
  set: workers now report sessions truthfully, so the synthesized
  `aborted_before_start` rows in `listDiskOnlySessions` /
  `buildDegradedFromDisk` would actively mislead operators (a worker
  briefly unreachable already produces the correct `worker_offline`
  envelope from the proxy).
- `cmd/coordinator_session_proxy.go` (HTML viewer over `mgmt_base_url`,
  used by the GitHub-comment URLs reporter posts) is **left in place**
  for Phase 2: it serves a different surface (HTML, not JSON) than the
  new audit-url-driven proxy, and removing it would break existing
  comment-link URLs. Phase 3 picks the consolidation path.
- Web UI is unchanged — it still hits the coordinator's
  `/api/v1/sessions[/...]` paths; only the implementation moved.

#### Behavior of pre-bundle (Apr-29 / Apr-30) legacy sessions

Sessions that pre-date the v0.5 cutover have rows in the coordinator's
local DB but their owning worker either no longer exists in the
`workers` table or never carried an `audit_url`. The `Resolve` chain
flags both as Local=true so a session-detail click still hits the
in-process handler and reads from the coordinator's own
`sessions/<id>/` directory. They DO disappear from the **listing**
endpoint, however: `/api/v1/sessions` now fans out only to live
workers, and the coordinator-local synthesis fallback is off. This is
intentional and called out in the release notes — those sessions are
days old, archival-only, and adding a hidden coordinator-local listing
branch for the long tail would re-introduce the disk-walk path Phase 3
is about to delete. If an operator needs to find one, they can still
hit `/api/v1/sessions/<id>` directly (the Local fallback path) using
the URL the reporter already posted into the original GitHub comment.

#### What Phase 3 still needs to do

- Delete `listDiskOnlySessions`, `buildDegradedFromDisk`,
  `StatusAbortedBeforeStart`, `degradedReasonNoDBRow`, the disk-walk
  metadata reader, and their tests once the legacy data has aged out
  enough that no operator playbook references them.
- Consolidate or delete `cmd/coordinator_session_proxy.go` (HTML
  viewer over `mgmt_base_url`). Either redirect
  `/workers/{id}/sessions/{sid}` → `/sessions/{sid}` (SPA canonical)
  and delete the proxy + worker-side mgmt HTML route, or refactor the
  proxy onto `audit_url` after the worker audit server grows an HTML
  view.
- Drop the coordinator's `sessions` table (and the legacy
  `agent_sessions` view) once Phase 3's session-list rewrite no longer
  reads from them. Migration deferred to its own ADR — there is still
  one cross-table read in `auditapi.handler` for issue detail pages
  (`store.QueryAgentSessions`) that needs replacement before the
  table can go.
- Wire `Handler.InvalidateCache()` into the coordinator's
  `worker_registered` eventlog hook for sub-second freshness on new
  worker registration, then drop the exported method if no other
  caller emerges.
- Frontend offline UX: surface `X-Workbuddy-Worker-Offline` (and the
  per-detail 503 `degraded_reason: worker_offline`) as a banner in
  the SPA so operators see the difference between "worker briefly
  unreachable" and "session truly missing".
- Search the codebase for "Phase 3" / "REQ-121" comments planted by
  this commit (in `internal/sessionproxy`, `cmd/coordinator.go`,
  `cmd/coordinator_session_proxy.go`) and act on each one.

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
