# Workflow Presets

Workbuddy workflows are **configurable and multi-preset**. When a repo is opted
into automated development, you can pick a built-in preset or author your own.
This is the model decided in
`docs/decisions/2026-06-06-runtime-strategy-and-convergence.md` §4 (which amends
the earlier "exactly two agents" decision: the minimalism principle still holds
*within* a workflow, but the role catalog is now **workflow-scoped**).

## How selection works

Each workflow file (`.github/workbuddy/workflows/*.md`) declares a trigger
label in its YAML frontmatter:

```yaml
trigger:
  issue_label: "workbuddy"
```

The state machine selects the workflow whose `trigger.issue_label` is present on
the issue (`internal/statemachine/statemachine.go`). The loader rejects two
workflows that claim the same trigger label
(`validateWorkflowTriggerConflicts` in `internal/config/loader.go`), so the
mapping from label → preset is unambiguous, per repo.

## Built-in presets

### `default` — dev → review → merge

Trigger label: `workbuddy`. States:

```
developing → (synthesizing) → reviewing → merging → merged
```

- `developing` (dev-agent): produces the artifact, opens a PR. Multi-rollout
  fan-out goes to `synthesizing`; single-rollout goes straight to `reviewing`;
  missing acceptance criteria goes to `blocked`.
- `synthesizing` (review-agent, `mode: synthesize`): conditional
  rollout-reduction branch; reduces fan-out siblings to one and advances to
  `reviewing`.
- `reviewing` (review-agent): verifies each acceptance criterion; advances to
  `merging` or sends back to `developing`.
- `merging` (merge-agent): rebases onto main, runs a quality check,
  squash-merges, closes the issue; advances to `merged` or sends back to
  `developing`.
- `merged`: terminal.
- `blocked`: no agent runs; a human rewrites the issue and flips it back to
  `developing`.
- `failed`: terminal (recognized by the schema; the Go runtime records
  retry/failure intent rather than writing it directly).

Roles used: `dev`, `review`, `merge`.

### `engineering` — spec → spec-review → test-gen → dev → review → merge

Trigger label: `engineering`. States:

```
specifying → spec_review → test_generating → developing → reviewing → merging → merged
```

Spec-driven: tests are generated from the spec **before** any implementation
exists. `spec-review` hands off to a dedicated `test_generating` stage
(test-gen-agent writes integration tests that must FAIL against an empty
implementation), then the dev agent implements against those tests, and the
review/merge tail is identical to the default preset. Back edges:
spec_review→specifying, reviewing→developing, merging→developing,
blocked→{specifying,developing}; `failed` terminal.

Roles used: `dev`, `review`, `merge`, plus the spec / spec-review / test-gen
agents.

## Authoring a custom preset

Drop a new `.github/workbuddy/workflows/<name>.md` with:

1. Frontmatter `name`, `description`, a **unique** `trigger.issue_label`, and
   the `max_retries` / `max_review_cycles` caps.
2. A fenced ```` ```yaml ```` `states:` block describing the state machine.
   Every `agent:` (or `agents:`) referenced must resolve to an agent in
   `.github/workbuddy/agents/`, and a workflow with fallback edges must declare
   a terminal `failed` state.

Validate with `workbuddy validate` — it resolves agent references, checks
trigger-label uniqueness, and lints the state graph. The repo's built-in
presets and the Helm chart's bundled presets
(`deploy/helm/workbuddy/templates/config-configmap.yaml`) are kept in parity
with each other.

## Source of truth

- Repo presets: `.github/workbuddy/workflows/default.md`,
  `.github/workbuddy/workflows/engineering.md`.
- `workbuddy init` scaffold: `cmd/initdata/workflows/default.md` (+ matching
  `cmd/initdata/agents/*.md`).
- Helm-bundled presets:
  `deploy/helm/workbuddy/templates/config-configmap.yaml`.
- Role catalog validity: `internal/validate/cross_refs.go` (`ValidRoles`) and
  `schemas/agent.schema.json`.
