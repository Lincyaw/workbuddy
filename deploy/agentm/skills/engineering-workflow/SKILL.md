---
name: engineering-workflow
description: >
  Overview of the engineering pipeline and how each stage connects.
  Use when an agent is confused about what stage it's in, what it
  should do next, or how its output feeds into the next stage.
  Trigger on: engineering workflow, pipeline, what stage am I in,
  what comes next, workflow overview, 工程流程, 开发流程,
  pipeline stage, stage transition.
---

# Engineering Workflow Overview

The engineering pipeline runs from requirement to merged code. Each
stage has a specific agent, clear entry/exit conditions, and a
feedback loop that sends work back upstream when quality gates fail.

## Pipeline

```
   ┌─────────────┐
   │ Requirement  │  (human writes issue with goal description)
   └──────┬───────┘
          │ label: engineering
          ▼
   ┌─────────────┐     reject: spec incomplete
   │  Specifying  │◄────────────────────────────┐
   │ (spec-agent) │                             │
   └──────┬───────┘                             │
          │ label: status:spec-review           │
          ▼                                     │
   ┌─────────────┐                              │
   │ Spec Review  │─────────────────────────────┘
   │(spec-review) │
   └──────┬───────┘
          │ label: status:developing
          ▼
   ┌─────────────────────────────────────┐
   │          Developing                  │
   │                                      │
   │  ┌──────────────┐ ┌──────────────┐  │
   │  │ test-gen     │ │ dev-agent    │  │  reject: tests fail
   │  │ (writes      │ │ (implements  │  │  or review rejects
   │  │  tests from  │ │  code from   │◄─┤
   │  │  spec)       │ │  spec)       │  │
   │  └──────────────┘ └──────────────┘  │
   │       both run in parallel           │
   └──────┬──────────────────────────────┘
          │ both complete, label: status:reviewing
          ▼
   ┌─────────────┐                       │
   │  Reviewing   │──────────────────────┘
   │(review-agent)│     reject: AC fails
   └──────┬───────┘
          │ all ACs pass
          │ label: status:done
          ▼
   ┌─────────────┐
   │    Done      │  (merge to main, close issue)
   └─────────────┘
```

## Stage responsibilities

### Specifying (spec-agent)

**Input**: Issue with a goal description (possibly vague).
**Output**: Structured spec comment with Interface, Acceptance
Criteria (AC-1..N), and Design Notes.
**Exit condition**: Spec posted on issue, label → spec-review.
**Feedback on rejection**: Spec-review tells you which ACs are
untestable and which interfaces are underspecified. Fix those
specific items.

Read the `spec-writing` skill for detailed guidance on what
makes a good spec.

### Spec Review (spec-review-agent)

**Input**: Issue with spec in body or comments.
**Output**: Per-AC testability verdict table.
**Exit condition**: All ACs testable → label → developing.
Or: specific ACs untestable → label → specifying with feedback.

Read the `acceptance-criteria` skill for testability rules.

### Developing (test-gen-agent + dev-agent, parallel)

Two agents run simultaneously on the same branch:

**test-gen-agent**:
- Reads the spec's ACs
- Writes integration tests (one per AC minimum)
- Verifies tests FAIL without implementation
- Does NOT write any implementation code

**dev-agent**:
- Reads the spec's Interface and ACs
- Implements the code
- Does NOT write the AC-derived tests (test-gen handles that)
- May write additional unit tests for internal logic

Both push to the same branch. When both complete, the state
advances.

Read the `test-design` skill for test writing conventions.

**Exit condition**: Both agents complete, label → reviewing.

### Reviewing (review-agent)

**Input**: Branch with implementation + pre-generated tests.
**Output**: Per-AC verdict (PASS/FAIL/CANNOT JUDGE) + code
quality findings.
**Exit condition**: All ACs PASS → label → done.
Or: any AC FAIL → label → developing with per-AC feedback.

Read the `code-review-standards` skill for review process and
verdict format.

### Done

Merge the PR, close the issue. This stage is mechanical — no
agent judgment required.

## Feedback loops

Three feedback loops operate at different timescales:

### 1. Spec loop (specifying ↔ spec-review)

**What it catches**: Untestable ACs, missing interfaces, vague
requirements.
**Cost of a rejection**: One spec-agent session (~minutes).
**Why it matters**: A bad spec that escapes to developing costs
N parallel dev sessions + test-gen + review cycles, all producing
the wrong thing. This is the highest-leverage gate.

### 2. Dev-review loop (developing ↔ reviewing)

**What it catches**: Code that doesn't satisfy ACs, quality issues.
**Cost of a rejection**: One dev-agent session (potentially
expensive with parallel rollouts).
**Feedback format**: Per-AC verdict with specific failing tests
and error messages. The dev agent on the next round sees exactly
which ACs to fix.
**Cap**: max_review_cycles (default 3). After 3 dev→review
round-trips, the issue is blocked for human intervention.

### 3. Turn-level loop (inside each agent session)

**What it catches**: Agent stuck (repeating same failed command),
agent idle (no productive actions for N turns).
**Mechanism**: Behavioral observation by the agent runtime (AgentM
atoms or session-level control loop).
**Feedback**: Injected message: "You've tried the same approach
3 times without progress. Try a different approach."

## Parallel development

When the issue specifies `rollouts: N` (via label `rollouts:N`),
the developing state dispatches N copies of the dev-agent alongside
one test-gen-agent. All N+1 sessions run in parallel.

At review time, the reviewer:
1. Runs the test-gen tests against each dev branch
2. Eliminates branches where tests fail
3. Selects the best passing branch based on code quality

This is multi-sampling: run N implementations, select the best.
Strictly better than sequential retries when budget allows.

## Labels

| Label | Meaning |
|---|---|
| `engineering` | Issue enters the engineering workflow |
| `status:specifying` | Spec-agent is writing/revising the spec |
| `status:spec-review` | Spec-review-agent is validating the spec |
| `status:developing` | Test-gen + dev agents are working in parallel |
| `status:reviewing` | Review-agent is running tests and reviewing |
| `status:done` | All ACs pass, PR merged |
| `status:blocked` | Waiting for human intervention |
| `rollouts:N` | Request N parallel dev sessions |

## When to use this workflow vs default

| Use engineering workflow when | Use default workflow when |
|---|---|
| The task involves new interfaces or APIs | The task is a simple bug fix with obvious scope |
| Multiple acceptance criteria need verification | One clear criterion: "this test should pass" |
| You want test-gen before implementation | Tests already exist or aren't needed |
| Parallel development is worth the cost | A single dev session is sufficient |
| Spec quality matters (long-lived code) | Quick fix, throwaway, or experiment |
