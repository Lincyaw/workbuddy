# Agent Role Consolidation: 9 → 2

- Date: 2026-04-15
- Status: accepted

## Context

Workbuddy's agent catalog previously registered 9 agents:

- `dev-agent`, `codex-dev-agent`
- `review-agent`, `codex-review-agent`
- `triage-agent`
- `docs-agent`
- `dependency-bump-agent`
- `release-agent`
- `security-audit-agent`

Each one had its own Markdown definition, some had their own
`output_contract` JSON schemas, and they were dispatched via a mix of
`status:*` labels, `type:*` labels, and `issue_created` events. This
gave the appearance of rich specialization, but in practice most of
the agents were minor variants of "produce an artifact from an issue"
or "read an issue and classify it".

## Decision

Collapse the catalog to exactly two agents:

- `dev-agent` — triggers on `status:developing`; produces whatever
  artifact (code, docs, dependency bump, report, release notes, …)
  satisfies the issue's `## Acceptance Criteria`; then flips the
  label to `status:reviewing`. If the issue is missing acceptance
  criteria, it flips to `status:blocked`.
- `review-agent` — triggers on `status:reviewing`; evaluates the
  artifact against each acceptance criterion. All pass → `status:done`.
  Any fail → back to `status:developing` with a feedback comment.

Runtime (`claude-code` / `codex`) becomes a
repo-level config override on these two agents, not its own catalog
entry.

## Rationale

This decision applies three auto-harness first principles.

### Quality over quantity

Each agent's existence has to pass the "what would be lost without
it" test. `codex-dev-agent` and `codex-review-agent` only varied from
their Claude siblings by runtime — runtime is an implementation
detail, not an identity, so it belongs on the config side as an
override. `docs-agent`, `dependency-bump-agent`, `release-agent`, and
`security-audit-agent` are all instances of "read an issue, produce
an artifact that satisfies the stated need" — that is exactly
`dev-agent`'s contract. Splitting by content domain added catalog
surface area (more Markdown files, more schemas, more trigger labels
to keep consistent) without adding any new capability.

### Deliberate execution

`triage-agent` was doing a human's job: reading a freshly-filed issue
and reverse-engineering acceptance criteria from the prose. Humans
write better acceptance criteria than an LLM can guess from a loose
bug report. Moving triage into the issue template — a
`## Acceptance Criteria` section the reporter fills in — replaces a
probabilistic LLM step with a deterministic form, and makes the
downstream dev/review contract sharper because both sides now point
at the same anchored list.

### Surface problems early

With 9 agents there were silent hazards: multiple agents could match
the same label, `type:*` triggers could re-fire on label edits, and
inconsistencies between an agent's `output_contract` and what it
actually emitted went unnoticed. With only 2 roles, misrouting is
structurally impossible: a `status:developing` label has exactly one
handler and a `status:reviewing` label has exactly one handler. Any
future extension has to justify itself against that simplicity.

## Consequences

- **(a) Runtime switching moves to repo-level config override.**
  Repos that prefer Codex now override `runtime` on `dev-agent` /
  `review-agent` in their own `.github/workbuddy/config.yaml`, rather
  than registering a parallel `codex-*` agent.
- **(b) Triage moves to the issue template.** The repository-level
  issue template must carry a `## Acceptance Criteria` section. If
  that section is missing or empty when `dev-agent` runs, the agent
  flips the issue to `status:blocked` and comments asking the
  reporter to fill it in.
- **(c) Policy (sandbox / approval / model) moves to
  Coordinator-side dynamic dispatch.** Instead of baking sandbox
  policy into each agent definition, the Coordinator will choose
  policy at dispatch time based on issue labels (e.g. a
  `needs:sandbox` label forces the stricter profile). Implementation
  is deferred; the `policy` field on agents remains the fallback
  until then.
- **(d) Operational knowledge migrates to business repos.** Concrete
  dev-loop commands (`go build`, `go test`, `pnpm run lint`, PR
  reconciliation, etc.) live in each business repo's own `CLAUDE.md`
  and `.claude/skills/`. Workbuddy's agent prompts describe the
  *orchestration contract* only; they no longer carry per-stack
  command lists.

## Links

- `docs/implemented/agent-catalog.md`
- `docs/implemented/current-config-workflow-and-agents.md`
- `docs/implemented/agent-schema-vnext.md`
