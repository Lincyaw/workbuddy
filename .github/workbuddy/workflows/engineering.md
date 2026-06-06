---
name: engineering
description: Spec-driven engineering workflow with parallel test generation and development
trigger:
  issue_label: "engineering"
max_retries: 3
max_review_cycles: 3
---

## Engineering Workflow

Five-state pipeline applied to every issue labeled `engineering`. Adds
spec writing, spec review, and parallel test generation on top of the
default workflow's develop/review loop.

The key difference from the default workflow: tests are generated from the
spec BEFORE any implementation exists. A dedicated test-gen agent reads the
acceptance criteria and writes integration tests that must FAIL against an
empty implementation. The dev agent implements code in parallel, unaware of
the test files being written. When both complete, the review agent runs the
pre-generated tests against the implementation and does a per-AC structured
review.

```yaml
states:
  specifying:
    enter_label: "status:specifying"
    agent: spec-agent
    transitions:
      "status:spec-review": spec_review
      "status:blocked": blocked

  spec_review:
    enter_label: "status:spec-review"
    agent: spec-review-agent
    transitions:
      "status:developing": developing
      "status:specifying": specifying

  developing:
    enter_label: "status:developing"
    agents:
      - test-gen-agent
      - dev-agent
    join:
      strategy: all_passed
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
    transitions:
      "status:specifying": specifying
      "status:developing": developing

  done:
    enter_label: "status:done"
```

### State graph

```
    specifying ◄──────────── blocked ◄──── (dev/test-gen: missing criteria)
         │  ▲                   ▲
         │  │ (spec rejected)   │
         ▼  │                   │
    spec_review                 │
         │                      │
         │ (spec approved)      │
         ▼                      │
    developing ─────────────────┘
    (test-gen + dev in parallel)
         │  ▲
         │  │ (review: any criterion fails; retry, max 3)
         ▼  │
     reviewing ──► done (all criteria pass)
```

Spec agent: reads the issue for a requirement description, writes a
structured spec with `## Interface` and `## Acceptance Criteria` sections,
then transitions to spec-review.

Spec-review agent: validates the spec has testable ACs and defined
interfaces. Approves (developing) or rejects with feedback (specifying).

Developing state: dispatches test-gen-agent and dev-agent simultaneously via
the `agents:` list. The test-gen agent writes integration tests from the
spec's ACs without seeing any implementation. The dev agent implements the
code without writing tests. Both must complete (`join: all_passed`) before
the state advances.

Review agent: runs the pre-generated tests against the implementation, then
does per-AC structured review. Passes (done) or sends back with feedback
(developing).

The `max_review_cycles` cap (default 3) applies to the developing/reviewing
loop the same way as in the default workflow. The specifying/spec_review
loop is capped by `max_retries`.
