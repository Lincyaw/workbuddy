# K8s-ification + AgentM Runtime + OTel End-to-End Tracing

- Date: 2026-05-13
- Status: accepted
- Target versions: v0.5 (in-process changes), v0.6 (K8s path)

## Context

Workbuddy today runs as `coordinator + worker` processes on hosts (systemd / `serve` mode), dispatches agents as host subprocesses (`claude-code`, `codex`), and persists state in SQLite. OTel tracing landed in 8b72e81 covering Go-internal spans across coordinator/worker/pipeline.

Three pressures motivate the next step:

1. **Service-ification.** Helm-chart deployment so workbuddy can be scheduled into the same cluster as other internal services.
2. **Per-repo execution environments.** Each repo should be developed in its own dev container image; today the agent inherits whatever the worker host has installed.
3. **Trace continuity.** A given issue's lifecycle (label → dispatch → agent → PR → review → merge) should be observable as one trace, not scattered across disjoint operations.

Two adjacent codebases inform the design:

- **AgentM** (`../AgentM`): Python pluggable agent SDK, OTel-aware.
- **agent-env** (`../agent-env`, a.k.a. ARL-Infra): K8s operator with Gateway API, SandboxSession abstraction, per-image isolation. Already has its own Helm chart.

Also:

- **AegisLab** (`../aegis/AegisLab`): provides shared MySQL and OTel collector in-cluster — workbuddy will reuse these rather than ship its own.

## Design principles

- The systemd path and the K8s path coexist as parallel deployment modes. K8s is additive; systemd users see no regression.
- K8s mode pays the cost of breaking changes (DB engine, agent isolation model) in exchange for clean isolation and observability.
- agent-env is used **thin**: only as a sandbox provider. Warm pool stays off; workbuddy's per-issue runs are minutes-long and don't benefit from sub-second pod-start latency.
- Agent boundaries are decided by **execution location**, not by runtime name (with one exception, noted in Block 2).

## Block 1 — AgentM as a third runtime

**Changes**

- Add `agentm` to the `runtime` enum alongside `claude-code` / `codex`.
- Define AgentM invocation contract:
  - Inputs: issue context, acceptance criteria, repo workspace path, `TRACEPARENT` env var.
  - Outputs: structured JSON containing `success | fail`, suggested `next_label`, artifact path, session log path.
- AgentM's CLI mode must accept the above inputs and emit a session artifact in the format workbuddy's session-data layer expects (conversation events + tool-call history).
- Trace propagation: AgentM's OTel SDK reads `TRACEPARENT` and continues the parent span.

**Cross-repo work**: small adapter PR to AgentM if its CLI contract doesn't already match.

## Block 2 — agent-env as execution environment

### Two execution modes

| Mode                  | Who handles git / `gh issue edit` | Who holds credentials       |
|-----------------------|-----------------------------------|------------------------------|
| Self-managed          | Agent (Agent-as-Router preserved) | Agent process               |
| Coordinator-managed   | Coordinator                       | Coordinator only            |

Coordinator-managed mode breaks the original "agent calls `gh issue edit` to drive the state machine" contract. Agent instead returns a structured result; coordinator executes the corresponding git push, PR creation, and label change.

### Mode selection (deployment matrix)

| runtime     | systemd (host)  | K8s (sandbox)        |
|-------------|------------------|----------------------|
| claude-code | self-managed     | **not supported**    |
| codex       | self-managed     | **not supported**    |
| agentm      | not supported    | coordinator-managed  |

Rationale for refusing claude-code/codex inside sandbox: anyone reaching for K8s mode is buying isolation; allowing tokens-in-sandbox would defeat the value proposition. If real demand emerges later, revisit.

### Architectural consequences

- **Single-process model in K8s.** Coordinator absorbs the original worker's scheduling logic (claim, cycle counter, stale-claim sweep). The independent `workbuddy worker` binary is for systemd only.
- **Workspace lifecycle.** Each session ephemerally `git clone`s into the sandbox, destroyed on session end. No PVC caching at v0.6; revisit only if clone latency becomes a real complaint.
- **No credentials in sandbox.** No PAT injection. The `gh`/`git` write surface is concentrated in the coordinator process.
- agent-env Gateway is called from coordinator with `create session(image, env) → exec → fetch artifacts → destroy`. The image is chosen per repo (config-declared or inferred from `.devcontainer/`).

## Block 3 — Workbuddy Helm chart

### Storage

- **SQLite → MySQL.** The DB layer is abstracted behind an interface; SQLite stays as the systemd default, MySQL is the K8s default.
- MySQL is **reused from AegisLab**; workbuddy's chart exposes `mysql.host` / `user` / `pass` / `db` values, no embedded MySQL.
- Coordinator runs **single-replica** with `strategy: Recreate`. Brief restart-window outages are accepted. Horizontal scale-out is deferred (would require leader election or pervasive optimistic locking — not justified at current scale).

### Chart layout

- `workbuddy` and `agent-env` are **independent Helm releases** with values that reference each other (Gateway endpoint, OTel endpoint, MySQL DSN). Not bundled as subchart — agent-env has its own release cadence.
- Agent configs (`.github/workbuddy/agents/*.md`) injected via ConfigMap. Updates require `helm upgrade` + pod restart. git-sync sidecar deferred.
- Gitea token in K8s Secret. One org-level token; no per-repo PAT scoping (internal trust boundary).
- Service: ClusterIP + Ingress (webui + webhook entrypoint).

### Migration tool

- `workbuddy db migrate --from sqlite --to mysql` for one-shot migration of existing installations.

## Block 4 — OTel end-to-end

### Collector

- Reuse AegisLab's collector. Chart exposes `otel.endpoint`. No collector bundled with workbuddy.

### Span coverage

- **Entrypoint spans** (root): webhook intake and each poll cycle in coordinator.
- **In-process**: coordinator / worker / pipeline (already present, 8b72e81).
- **Cross-process to sandbox**: coordinator passes `Env: {TRACEPARENT: <ctx>}` when calling agent-env Gateway's session-create. agent-env already supports env injection (`pkg/gateway/types.go:79`); no upstream PR required.
- **Inside sandbox**: AgentM continues the parent span via its OTel SDK. claude-code / codex remain opaque (not in scope; they don't run in sandbox anyway).

### Long-lifecycle trace correlation

Issues and PRs live for hours-to-days and span many discrete workbuddy operations. OTel spans can't stay open that long, so correlation is done by **trace_id reuse**, not by long-running spans.

- DB schema adds `issues.root_trace_id` and `prs.root_trace_id`.
- On first sighting of an issue, coordinator creates a brief root span (closed immediately), stores its `trace_id` in the issues row.
- Every subsequent operation on that issue (poll-hit, dispatch, git push, PR creation, label change, review) loads `root_trace_id` from DB, constructs a parent `SpanContext` with `(trace_id=stored, span_id=new, parent=root)`, and emits its span under the shared trace.
- PRs inherit `root_trace_id` from their parent issue.
- Required span attributes on every span: `issue.id`, `issue.number`, `repo`, `pr.number` (when applicable), `agent.role`, `agent.runtime`.

This produces sparse multi-hour trees in Jaeger/Tempo — supported by the format. If span volume per trace becomes unwieldy, fall back to span-links between shorter traces.

## Block 5 — Migration & rollout

### Backward compatibility

- All existing `.github/workbuddy/agents/*.md` configs (runtime=claude-code/codex) continue to work unchanged in systemd mode (self-managed).
- AgentM agents are new files; they only deploy in K8s mode.
- No forced migration. Users opt into K8s when ready.

### Documentation

- `workbuddy` skill adds K8s deployment section; clearly separates the two paths.
- `workbuddy deploy` CLI remains systemd-only. K8s users use standard `helm install`.

### Version split

- **v0.5** (no K8s dependency, ships independently):
  - DB-layer abstraction + MySQL backend
  - SQLite→MySQL migration tool
  - `agentm` runtime (still host-exec, no sandbox yet)
  - OTel `root_trace_id` persistence + cross-span correlation
- **v0.6** (depends on v0.5):
  - agent-env integration
  - Helm chart
  - Coordinator-managed dispatch path
  - Sandbox execution for AgentM

## Cross-repo dependencies

- **AgentM**: confirm CLI input/output contract matches; adapter PR if needed.
- **agent-env**: env injection already supported; no changes required. Helm chart consumed as-is.
- **AegisLab**: MySQL service + OTel collector referenced via values only; no upstream changes.

## Open items (deferred, not blocking)

- PVC-based workspace caching for slow-cloning repos.
- Coordinator multi-replica (requires leader election or optimistic-lock audit of dispatch path).
- git-sync sidecar for hot-reloading agent configs.
- claude-code / codex inside sandbox (only if real demand emerges).
- Span-links fallback if per-issue trace size grows unwieldy.
