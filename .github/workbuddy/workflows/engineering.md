---
name: engineering
description: Spec-driven engineering workflow (spec → spec-review → test-gen → dev → review → merge)
trigger:
  issue_label: "engineering"
max_retries: 3
max_review_cycles: 3
---

## Engineering Workflow

Built-in preset selected by the `engineering` trigger label (see
`docs/implemented/workflow-presets.md` for the preset model). It adds spec
writing, spec review,
and a dedicated test-generation stage on top of the default workflow's
develop → review → merge loop.

The key difference from the default workflow: tests are generated from the
spec BEFORE any implementation exists. A dedicated test-gen agent reads the
acceptance criteria and writes integration tests that must FAIL against an
empty implementation. The dev agent then implements code against those tests.
When development completes, the review agent runs the pre-generated tests
against the implementation and does a per-AC structured review; the merge agent
ships the result.

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
      "status:test-generating": test_generating
      "status:specifying": specifying

  test_generating:
    enter_label: "status:test-generating"
    agent: test-gen-agent
    transitions:
      "status:developing": developing
      "status:blocked": blocked

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
    transitions:
      "status:specifying": specifying
      "status:developing": developing

  failed:
    enter_label: "status:failed"
```

### State graph

```
    specifying ◄──────────── blocked ◄──── (spec/test-gen/dev: missing criteria)
         │  ▲                   ▲
         │  │ (spec rejected)   │
         ▼  │                   │
    spec_review                 │
         │                      │
         │ (spec approved)      │
         ▼                      │
    test_generating ────────────┤
         │                      │
         ▼                      │
    developing ─────────────────┘
         │  ▲
         │  │ (review/merge: send back; retry, max 3)
         ▼  │
     reviewing ──► merging ──► merged (terminal)
```

Spec agent: reads the issue for a requirement description, writes a
structured spec with `## Interface` and `## Acceptance Criteria` sections,
then transitions to spec-review.

Spec-review agent: validates the spec has testable ACs and defined
interfaces. Approves (test-generating) or rejects with feedback (specifying).

Test-gen agent: reads the spec's ACs and writes integration tests that FAIL
against the empty implementation, then transitions to developing.

Dev agent: implements the code so the pre-generated tests pass, opens a PR,
then transitions to reviewing.

Review agent: runs the pre-generated tests against the implementation, then
does per-AC structured review. Passes (merging) or sends back with feedback
(developing).

Merge agent: rebases onto main, resolves conflicts, runs a quality check,
squash-merges, closes the issue, transitions to merged (or back to developing
on a failed merge).

The `max_review_cycles` cap (default 3) applies to the developing/reviewing
loop the same way as in the default workflow. The specifying/spec_review
loop is capped by `max_retries`.
