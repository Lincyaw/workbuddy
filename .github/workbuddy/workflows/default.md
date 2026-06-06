---
name: default
description: Default dev → review → merge lifecycle for any workbuddy-tracked issue
trigger:
  issue_label: "workbuddy"
max_retries: 3
max_review_cycles: 3
---

## Default Workflow

Built-in preset selected by the `workbuddy` trigger label (see
`docs/implemented/workflow-presets.md` for the preset model and how to author
your own). The
state machine reacts only to label changes — it doesn't care whether a human
or an agent flipped the label. Bug vs feature distinction lives in optional
`type:*` classification labels, not in separate workflows — the execution path
is the same either way.

The lifecycle is dev → review → merge: the dev agent produces the artifact and
opens a PR, the review agent verifies it against the acceptance criteria, and
the merge agent rebases onto main, runs a quality check, squash-merges, and
closes the issue. `synthesizing` is a conditional rollout-reduction branch
between develop and review (kept for fan-out runs); single-rollout issues skip
straight to reviewing.

```yaml
states:
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      "status:synthesizing": synthesizing
      "status:reviewing": reviewing
      "status:blocked": blocked

  synthesizing:
    enter_label: "status:synthesizing"
    agent: review-agent
    mode: synthesize
    transitions:
      "status:reviewing": reviewing

  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      "status:merging": merging
      "status:developing": developing

  merging:
    enter_label: "status:merging"
    agent: merge-agent
    transitions:
      "status:merged": merged
      "status:developing": developing

  merged:
    enter_label: "status:merged"

  blocked:
    enter_label: "status:blocked"
    # no agent runs; waits for a human to rewrite the issue
    # (typically adding a proper `## Acceptance Criteria` section)
    # and flip the label back to status:developing.
    transitions:
      "status:developing": developing

  failed:
    enter_label: "status:failed"
```

`failed` 仍然是 workflow schema 中可识别的终态 label，但当前 Go runtime 不会在 retry 超限时直接写入
`status:failed` 或 `needs-human`；它只记录 retry/failure intent，后续 label 写回仍由 agent 或人工执行。

The `developing` state is conditional:
- if `rollouts > 1`, dev runs fan out and the successful sibling set must move to `status:synthesizing`;
- if `rollouts <= 1`, the legacy fast path stays `status:reviewing` with no synth step.

`max_review_cycles` (default 3) caps the orchestrator-level dev↔review
round-trip count: every developing→reviewing→developing increment counts as
one cycle. On cap-hit the Coordinator stops dispatching `dev-agent` and
`review-agent`, posts a needs-human comment with a rejection-trail digest
(assembled from existing `completed` events — no agent re-invocation), and
emits a `dev_review_cycle_cap_reached` event + alert. A heads-up alert fires
when `cycles == max_review_cycles - 1` so an operator can intervene
preemptively.

To resume work after a cap-hit (or any other manual block), a human flips
`status:blocked` → `status:developing` on the issue. The Coordinator
detects this label transition and **resets the cycle counter to zero**
(Option A semantics: "give the agent another shot"). The
blocked→developing transition itself does not count as a round-trip, so
the next genuine review→developing increments to 1, not cap+1.
`workbuddy issue restart` is still available for explicit, full-state
restarts that also clear `first_dispatch_at` (long-flight clock).

`status:merged` is the terminal label. The merge agent squash-merges the PR
and closes the issue; the state machine itself does not close issues or merge
PRs on behalf of agents.

### State graph

```
         ┌──────────── blocked ◄──── (dev: missing criteria)
         │                │
         │      (human rewrites issue)
         ▼                │
    developing ◄──────────┴───────────────┐
         │  ▲                             │
         │  │ (review/merge: send back)   │ (rollout fan-in)
         ▼  │                             │
   (synthesizing) ──► reviewing ──► merging ──► merged (terminal)
```

Dev agent: reads `## Acceptance Criteria`, produces the artifact, opens a PR,
flips to reviewing (or to synthesizing for multi-rollout fan-in, or to blocked
if criteria missing).
Review agent: verifies each criterion against the artifact, flips to merging or
back to developing.
Merge agent: rebases onto main, resolves conflicts, runs a quality check,
squash-merges, closes the issue, flips to merged (or back to developing on a
failed merge).
Any revisit of a state — including developing↔blocked — counts toward
max_retries; exceeding the limit will record retry/failure intent.
