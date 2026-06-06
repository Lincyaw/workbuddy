# Runtime Strategy Interface & Two-Era Convergence

- Date: 2026-06-06
- Status: accepted
- Amends: [2026-04-15-agent-role-consolidation.md](2026-04-15-agent-role-consolidation.md)
  (roles are now defined *by the selected workflow*, not fixed at two)
- Builds on: [2026-05-13-k8s-agentm-otel.md](2026-05-13-k8s-agentm-otel.md),
  [2026-05-01-session-data-ownership.md](2026-05-01-session-data-ownership.md)

## Context

Workbuddy's north star: it is a **harness for agent runtimes** that, using
closed-loop control thinking, automates the full DevOps loop. Mapped onto a
control system: the poller is the sensor, the state machine is the controller,
the agent runtime is the actuator, the issue labels are the plant state, and
`staleinference`/`operator`/`taskreaper`/`diagnose`/retry-caps are the
feedback/error-correction layer. The core loop
(`poller → statemachine → router → taskprep → worker`) is sound and carries no
architectural debt.

The debt lives at the **seams between two eras the codebase carries
simultaneously**:

- **host-exec / self-managed era** (v0.1–0.5): a local worker runs `claude` /
  `codex` subprocesses; the agent edits issue labels itself via `gh issue edit`
  from inside its own prompt; split-host deployments are stitched together with
  a session-viewing tunnel stack.
- **agentm / coordinator-managed era** (v0.6): a single K8s pod, sandbox
  execution delegated to agent-env, labels written Go-side via
  `internal/labelwriter`.

An architecture audit (2026-06-06) found that nearly every key surface is
doubled along this seam, and that the duplication is the real source of
accidental complexity:

- **4 runtime backends** (`claude-code`, `codex`, `agentm`, `gha`) selected by
  scattered `if RuntimeAgentM` / runtime-name branches rather than one
  abstraction. (`internal/runtime/registry.go`, `internal/runtime/agent_bridge.go`)
- **2 label-write paths**: agent `gh issue edit` (invisible to Go, unauditable)
  vs `internal/labelwriter` (Go-side, auditable, Gitea-capable). Gated per
  runtime, with slightly divergent semantics. (`internal/labelwriter/labelwriter.go`)
- **2 worker config models**: the coordinator already stores per-repo agent
  config (`internal/app/repo_runtime.go`, incl. `dev_container_image`), but the
  dispatch protocol ships only `AgentName`, so the worker re-resolves config
  from its **own** name-keyed local config (`internal/worker/distributed.go:94`).
  This blocks dynamic multi-repo onboarding and per-repo images.
- **split-host session stack** (`sessionproxy` + `wstunnel` + `resolver` +
  tunnel-client + `session_routes`, ~1000 LOC) that is a hot-path **no-op**
  (`Local=true` for every request) in the single-pod deployment.
- **catalog/doc contradiction**: CLAUDE.md states "only two roles, no
  test-agent", but the repo ships `spec-agent`, `spec-review-agent`,
  `test-gen-agent`; the Helm chart additionally ships `merge-agent`;
  `serve.go` defaults a `test` role with no backing agent; and three divergent
  workflows exist (`default.md` …→synthesizing→reviewing→**done** vs Helm
  default …→merging→**merged** vs `engineering.md`).
- **over-redundant feedback layer**: four stuck detectors with unaligned
  thresholds (staleinference 10m, operator 60s, taskreaper 2×timeout, diagnose
  on-demand) — staleinference can kill an agent ~110 min before diagnose would
  classify it stuck.

## Decisions

### 1. A single runtime Strategy interface; host-exec lives *inside* it

Introduce one runtime abstraction that the control loop talks to without
knowing which backend is behind it. `claude-code`, `codex`, and `agentm` (and
`gha` if retained — see §6) become peer implementations of this interface.
**host-exec is NOT removed** — it is a first-class implementation behind the
interface.

Rationale: we intend to **benchmark and compare runtimes** against each other,
so host-exec must remain a live, swappable peer. What gets eliminated is the
*leakage*: the scattered `if runtime == agentm` branches across dispatch, label
writing, session capture, and config wiring collapse into the strategy. External
code (poller, state machine, router, taskprep, reporter) becomes
runtime-agnostic.

The interface must expose, at minimum: launch/session lifecycle, label-write
capability (see §3), session-artifact capture, and a capability descriptor
(e.g. "needs host gh creds" vs "sandboxed via agent-env") so the claim filter
and benchmarking harness can reason about backends uniformly.

### 2. Stateless worker: per-task config travels over the wire

The coordinator resolves `(repo, agentName) → AgentConfig` (it already holds
this per-repo) and ships the **resolved config** in the dispatched task. The
internal `worker.Task` already has an `Agent *config.AgentConfig` field; extend
the wire DTO (`workerclient.Task`) the same way. The worker stops calling
`w.deps.Config.Agents[task.AgentName]` and uses `task.Agent`; workflow-state
metadata is supplied the same way instead of re-resolved locally.

Consequence: the worker becomes **stateless w.r.t. repo config**. A new repo is
onboarded by one `repo register` HTTP call (carrying its agents + per-repo
`dev_container_image`); it is immediately dispatchable with **no worker
redeploy, no config edit, no extra replica**. Per-repo dev images fall out for
free, and the `AGENTM_AGENT_ENV_IMAGE` `!exists` precedence trap in
`agent_bridge.go` disappears (no pod-global image to shadow per-repo values).

This is the highest-leverage change: it unlocks dynamic multi-repo onboarding
and per-repo images in one move.

### 3. Unified Go-side label writes for all runtimes

Label writing becomes a capability of the runtime Strategy, backed by
`internal/labelwriter` for **every** runtime (not just agentm). Agents no longer
edit labels via `gh issue edit` regardless of backend. This makes state
transitions uniformly auditable, removes the divergent two-phase vs atomic
semantics, and lets the WB-L001 linter / prompt constraints simplify.
(Self-managed agents keep `gh`/`git` for code work — only *label* writes move
Go-side.)

### 4. Workflows are configurable, multi-preset; roles are defined by workflow

Workflows are **not** collapsed to one. The product model: when a user opts a
repo into automated development, they can (a) pick from built-in workflow
presets, and (b) author their own. Built-in presets are shipped and versioned:
at least `default` (dev → review → merge) and `engineering` (spec → spec-review →
test-gen → dev → review → merge). Selection is by trigger label
(`WorkflowTrigger.IssueLabel`), per repo.

This **amends** the 2026-04-15 "exactly two agents" decision: the *minimalism
principle still holds within a workflow*, but the catalog is now
**workflow-scoped**. Roles are whatever the selected workflow declares. CLAUDE.md
must be updated to say "roles are defined by the active workflow" and to stop
asserting a global two-role cap / "no test-agent".

Cleanup obligations under this decision:
- Reconcile the three divergent workflow definitions into coherent, named,
  versioned presets (resolve `done` vs `merged` terminal, where `synthesizing`
  lives, whether `merge-agent` is in the default preset).
- Remove the unbacked `test` default role in `serve.go`, or give it a backing
  agent.
- Either wire `control/objective.go`'s `MergeObjective()` / `status:merging`
  into the preset that uses it, or remove it.

### 5. Single-pod is canonical; split-host session stack is feature-gated

The agentm single-pod (`serve`-in-K8s) deployment is the canonical target. The
split-host session-viewing stack (`sessionproxy`, `wstunnel`, `resolver`,
tunnel-client) is retained for genuine split-host deployments but
**feature-gated off** in single-pod mode so it is not a hot-path no-op. Session
reads short-circuit to the local audit handler when coordinator and worker share
a process.

### 6. Targeted cleanup

- **gha runtime**: confirm whether any deployment uses it. If yes, keep it as a
  Strategy implementation (§1) — it is a legitimate runtime to benchmark. If no,
  remove `gha_runner.go` (×2) and the session-starter callback.
- **Feedback thresholds**: unify staleinference / operator / taskreaper around a
  single shared `orphaned_after` + `check_interval` config so the detectors stop
  firing on drifted thresholds; diagnose stays an on-demand forensic tool.
- **Notification fan-out**: document (or de-dup across) the two parallel paths
  `alertbus→notifier` and `eventlog→hooks` that both consume `AlertEvent`.
- **Schema cruft**: remove or implement `FileSystemPermissionsConfig` /
  `ResourceLimitsConfig` ("deferred to v0.5", parse-only) and the unused
  `JoinConfig.MinSuccesses`.
- **Dual dependency gate**: collapse the redundant `dependency.IsBlocked()` check
  done in both the state machine and the router.
- **Cycle-cap duplication**: unify the dev↔review and synthesize re-entry
  counters under one cap abstraction.
- **Gitea read gap**: the poller is GitHub-only while labelwriter is
  Gitea-capable; make the read path host-kind aware (or document the
  single-backend-per-deployment constraint as enforced).

## Consequences

- The control loop becomes truly runtime-agnostic; adding/benchmarking a new
  runtime is implementing one interface, not threading branches through the
  codebase.
- Dynamic, single-worker, multi-repo onboarding with per-repo dev images becomes
  possible via `repo register` alone.
- State transitions become uniformly auditable.
- Workflows become a user-facing configurable surface (presets + custom),
  matching how users actually adopt automated development.
- Net code reduction at the seams (label paths, config re-resolution, single-pod
  tunnel no-op), without losing the ability to run or compare host-exec runtimes.

## Sequencing

1. §2 stateless worker / wire-config (unlocks dynamic multi-repo + per-repo image).
2. §1 runtime Strategy interface (absorb host-exec + agentm; fold in §3 label
   writes as a capability).
3. §4 workflow presets + catalog/doc reconciliation (update CLAUDE.md).
4. §5 single-pod feature-gate of the session tunnel stack; §6 gha decision.
5. §6 feedback-threshold unification and schema-cruft removal.

## References (audit findings, 2026-06-06)

- `internal/worker/distributed.go:94` — worker-local name-keyed config resolution.
- `internal/worker/task.go` — internal `Task.Agent` field exists; `workerclient.Task` ships only `AgentName`.
- `internal/runtime/registry.go`, `internal/runtime/agent_bridge.go:752,784` — runtime branching + `AGENTM_AGENT_ENV_IMAGE` precedence.
- `internal/labelwriter/labelwriter.go` — Go-side label writer (GitHub + Gitea).
- `internal/sessionproxy/`, `internal/wstunnel/` — split-host session stack, no-op in single-pod.
- `internal/staleinference/`, `internal/operator/detector.go`, `internal/diagnose/diagnose.go`, coordinator taskreaper — four unaligned stuck detectors.
- `.github/workbuddy/agents/{spec-agent,spec-review-agent,test-gen-agent}.md`, Helm `merge-agent`, `cmd/serve.go` `test` default role, `internal/control/objective.go` — catalog/doc contradiction.
- `.github/workbuddy/workflows/default.md` vs Helm `workflow-default.md` vs `engineering.md` — three divergent workflows.
- `internal/config/types.go:90-100,202-205` — deferred FS/resource fields, unused `JoinConfig.MinSuccesses`.
