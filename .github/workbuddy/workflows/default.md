---
name: default
description: Default 2-agent lifecycle for any workbuddy-tracked issue
trigger:
  issue_label: "workbuddy"
max_retries: 3
max_review_cycles: 3
---

## Default Workflow

Two-agent state machine applied to every issue labeled `workbuddy`. Humans
author issues with a `## Acceptance Criteria` section; agents decide
transitions by modifying issue labels via `gh issue edit`. The state machine
only reacts to label changes — it doesn't care whether a human or an agent
changed the label. Bug vs feature distinction lives in optional `type:*`
classification labels, not in separate workflows — the execution path is the
same either way.

```yaml
states:
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      "status:reviewing": reviewing
      "status:blocked": blocked

  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      "status:done": done
      "status:developing": developing

  blocked:
    enter_label: "status:blocked"
    # no agent runs; waits for a human to rewrite the issue
    # (typically adding a proper `## Acceptance Criteria` section)
    # and flip the label back to status:developing.
    transitions:
      "status:developing": developing

  done:
    enter_label: "status:done"

  failed:
    enter_label: "status:failed"
```

`failed` 仍然是 workflow schema 中可识别的终态 label，但当前 Go runtime 不会在 retry 超限时直接写入
`status:failed` 或 `needs-human`；它只记录 retry/failure intent，后续 label 写回仍由 agent 或人工执行。

`max_review_cycles` (default 3) caps the orchestrator-level dev↔review
round-trip count: every developing→reviewing→developing increment counts as
one cycle. On cap-hit the Coordinator stops dispatching `dev-agent` and
`review-agent`, posts a needs-human comment with a rejection-trail digest
(assembled from existing `completed` events — no agent re-invocation), and
emits a `dev_review_cycle_cap_reached` event + alert. A heads-up alert fires
when `cycles == max_review_cycles - 1` so an operator can intervene
preemptively. Use `workbuddy issue restart` to clear the counter after human
intervention.

`status:done` is the post-merge terminal label; the review-agent (or the human
who merged the PR) is responsible for closing the issue. The state machine
does not close issues on behalf of agents.

### State graph

```
         ┌──────────── blocked ◄──── (dev: missing criteria)
         │                │
         │      (human rewrites issue)
         ▼                │
    developing ◄──────────┘
         │  ▲
         │  │ (review: any criterion fails; retry, max 3)
         ▼  │
     reviewing ──► done (all criteria pass; issue close stays with merge owner)

Dev agent: reads `## Acceptance Criteria`, produces the artifact, flips to
reviewing (or to blocked if criteria missing).
Review agent: verifies each criterion against the artifact, flips to done or
back to developing. Closing the issue after merge remains the responsibility of
the review-agent or the human who merged the PR.
Any revisit of a state — including developing↔blocked — counts toward
max_retries; exceeding the limit will record retry/failure intent.
```
