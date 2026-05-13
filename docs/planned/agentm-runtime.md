# AgentM Runtime — invocation & output contract (v0.5)

- Status: contract only — config validation lands in v0.5 via issue [#315];
  actual dispatch wiring is tracked in issue **#319**.
- Design anchor: [docs/decisions/2026-05-13-k8s-agentm-otel.md](../decisions/2026-05-13-k8s-agentm-otel.md) (Block 1).
- Upstream: [AgentM](https://github.com/AgentM-dev/AgentM) (sibling repo `../AgentM`).

## What this document is

The `agentm` value of `runtime:` in an agent-config frontmatter selects the
AgentM pluggable agent SDK as the execution backend. This document is the
single source of truth for how workbuddy invokes `agentm` and what shape the
output must take, so that:

1. Config files that declare `runtime: agentm` validate cleanly (`workbuddy
   validate`, `workbuddy validate-docs --strict`).
2. The follow-up dispatch implementation (issue #319) has a written contract
   to code against.
3. AgentM contributors know exactly what workbuddy expects when the upstream
   CLI is adapted.

**Out of scope for v0.5.**
Sandboxed / coordinator-managed dispatch is v0.6 (see decision doc Block 2 and
issue [#319]). v0.5 only delivers the runtime enum entry, the JSON Schemas,
and validation.

## How `agentm` differs from `claude-code` / `codex`

| Aspect | `claude-code` / `codex` | `agentm` (v0.5 host-exec) | `agentm` (v0.6 sandbox) |
|---|---|---|---|
| Execution host | worker host subprocess | worker host subprocess | agent-env sandbox pod |
| Holds GitHub credentials | yes (PAT in env) | yes (PAT in env) | **no** — coordinator only |
| Drives state machine | self-managed: agent calls `gh issue edit` | self-managed: agent calls `gh issue edit` | coordinator-managed: workbuddy flips labels using `next_label` from the structured output |
| Structured output expected on stdout | no — labels move via `gh issue edit` | **yes** — RESULT line matching `schemas/agentm-output.schema.json` | yes — same schema; `gh issue edit` is forbidden |
| Workspace lifecycle | inherits worker working tree | inherits worker working tree | ephemeral clone in sandbox |
| Trace propagation | none today | `TRACEPARENT` env, AgentM OTel SDK continues parent span | same, via agent-env Gateway env injection |

In v0.5 the **self-managed** column applies. The structured output is still
required because it is the v0.6-forward contract: producing it now keeps the
two paths convergent and lets workbuddy's worker double-check the label flip
against `next_label` even in host-exec.

## Invocation contract (host-exec, v0.5)

Workbuddy's worker invokes AgentM as a subprocess. The contract below is what
issue #319 must implement and what the upstream `agentm` CLI must accept.

### Binary

- Expected on `$PATH`: `agentm` (matches `runtimeBinaries["agentm"]` in
  `internal/validate/semantics.go`).
- Recovery-floor fallback (`agentm --no-extensions`) is acceptable when an
  operator forces it, but workbuddy itself always invokes full mode.

### Arguments

```
agentm run \
  --workspace <abs path to repo workspace> \
  --task-file <abs path to a JSON file workbuddy writes pre-launch> \
  --session-log <abs path AgentM should write its session log to> \
  --result-file <abs path AgentM should write its RESULT JSON to>
```

- `--workspace` is the per-task workspace the worker prepared (clone +
  branch checkout). AgentM must `cd` here before doing any tool calls.
- `--task-file` is a JSON document workbuddy writes describing the issue
  (number, title, body, AC text, labels, prior review comments) — see the
  Inputs section below for fields.
- `--session-log` is where AgentM's `observability` atom MUST emit the
  OTel-flavored JSONL session log (so workbuddy's session-data layer can
  ingest it).
- `--result-file` is where AgentM MUST write its structured result on exit.
  Workbuddy reads this file on subprocess exit (status 0 OR non-zero) and
  validates it against `schemas/agentm-output.schema.json`.

### Environment

Variables workbuddy injects (additive to whatever the worker process
inherits):

| Var | Required | Notes |
|---|---|---|
| `TRACEPARENT` | yes when OTel is enabled | W3C TraceContext (`00-<trace_id>-<span_id>-<flags>`) for the current dispatch span. AgentM's OTel SDK reads it and continues the parent span (see decision doc Block 4). |
| `WORKBUDDY_RUN_ID` | yes | Workbuddy's internal dispatch id; AgentM echoes it on every emitted span as `workbuddy.run_id`. |
| `WORKBUDDY_ISSUE_NUMBER` | yes | For span attributes and audit logs. |
| `WORKBUDDY_REPO` | yes | `owner/repo` form. |
| `GH_TOKEN` | host-exec only | Same PAT codex/claude-code receive today. **In v0.6 sandbox mode this MUST NOT be injected.** |
| `ANTHROPIC_API_KEY` / provider keys | provider-dependent | AgentM expects its provider extension to find these in env. |

### stdin

Workbuddy does not write to AgentM's stdin. The task description is delivered
via `--task-file` instead so that arbitrarily large issue bodies and AC text
don't have to be argv-safe.

### Task file (JSON document workbuddy writes pre-launch)

```jsonc
{
  "schema_version": 1,
  "issue": {
    "number": 315,
    "title": "v0.5: Add agentm runtime + invocation contract",
    "body": "…full issue body…",
    "labels": ["status:triage", "type:feature"],
    "comments": [/* prior review-agent comments, ordered */]
  },
  "acceptance_criteria": "AC-1-1: …\nAC-1-2: …",
  "repo": "Lincyaw/workbuddy",
  "branch": "v0.5/agentm-runtime-contract",
  "workspace": "/var/lib/workbuddy/work/<run-id>",
  "agent_role": "dev",
  "scenario": "general_purpose"
}
```

The exact field set will be finalized when #319 lands; this document MUST be
updated in the same PR if the field set changes.

### Expected workspace state on entry

- Clean working tree on the target branch (the worker has already done
  `git checkout` / `git pull`).
- Git identity configured (`user.name`, `user.email`).
- The worker's standard config-only files (`.workbuddy/`, etc.) are *not*
  guaranteed to be inside the workspace — AgentM must not rely on them.

## Output contract

AgentM MUST emit a JSON object at the path passed via `--result-file`,
matching [`schemas/agentm-output.schema.json`](../../schemas/agentm-output.schema.json).
Fields:

| Field | Required | Notes |
|---|---|---|
| `success` | always | `true` if the run satisfied the task. |
| `next_label` | always | `status:<lowercase>` literal — the label workbuddy should add. In self-managed v0.5 this should match what AgentM itself flipped via `gh issue edit`; in v0.6 sandbox mode AgentM does *not* flip labels and `next_label` is the sole signal. |
| `artifact_path` | optional | Absolute path to the produced patch/diff/output. Recommended on success. |
| `session_log_path` | required when `success=true` | Path to AgentM's session JSONL. |
| `failure_reason` | required when `success=false` | Short single-line reason; long rationale belongs in the session log. |

### Validation behavior (post-#319)

When dispatch lands, workbuddy will:

1. Refuse to consider an `agentm` run finished if `--result-file` is missing or
   the JSON fails `schemas/agentm-output.schema.json` validation. The run is
   classified as `infra-failure`, not `task-failure`.
2. Compare `next_label` against the label the issue actually ended up with
   (in self-managed mode) and emit a warning if they disagree.
3. Forward `session_log_path` to the session-data ingester.

## Forward-compatibility (v0.6 preview)

The same schema is reused for coordinator-managed dispatch. The only operational
delta:

- `GH_TOKEN` is **not** in env.
- AgentM MUST NOT shell out to `gh` or `git push`. The coordinator performs
  the post-run `git push` + `gh pr create` + label flip using `next_label`.
- The workspace is an ephemeral sandbox clone; nothing persists between runs.

Because the output contract is identical, no schema bump is expected when
v0.6 ships. If a bump *is* needed (e.g. AgentM wants to return multiple
artifacts), the schema MUST add an optional field rather than break v0.5
consumers.

## Where this is wired in v0.5

| Surface | File | Behavior |
|---|---|---|
| Runtime enum | `internal/config/loader.go` — `RuntimeAgentM` | added to `validRuntimes` and `publicRuntimes`. |
| Policy matrix | `internal/config/loader.go` — `normalizeAgentConfig` | accepts `runtime: agentm` with the same sandbox/approval set as `codex`. |
| Cross-ref validator | `internal/validate/cross_refs.go` — `ValidRuntimes` | recognizes `"agentm"`. |
| Binary check | `internal/validate/semantics.go` — `runtimeBinaries` | maps `agentm → agentm`. |
| Agent frontmatter schema | `schemas/agent.schema.json` | enum includes `"agentm"`. |
| Output schema | `schemas/agentm-output.schema.json` | this file. |
| Dispatch | *not in v0.5* | tracked in issue #319. |

## Out-of-scope reminders

- **No dispatch.** A worker invoked with `runtime: agentm` today will fail at
  dispatch time — that wiring is #319. v0.5 only guarantees the config
  validates and the contract is documented.
- **No sandbox.** agent-env integration is v0.6.
- **No new agents.** This work does not add agent configs under
  `.github/workbuddy/agents/` for AgentM. Catalog policy (dev + review only)
  is unchanged.

[#315]: https://github.com/Lincyaw/workbuddy/issues/315
[#319]: https://github.com/Lincyaw/workbuddy/issues/319
