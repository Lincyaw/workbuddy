# Writing Good Issues for Workbuddy

This guide helps humans write issues that agents can successfully process.
The quality of the issue directly determines whether the agent cycle completes
autonomously or gets stuck in retries.

## The contract

Workbuddy's 2-agent model has one hard contract:

> Every issue must contain a section titled exactly `## Acceptance Criteria`
> with individually verifiable criteria.

If this section is missing, dev-agent sets `status:blocked` and waits for
a human to fix the issue. If the criteria are vague, the cycle wastes retries
on misinterpretation.

## Required labels

An issue must have BOTH labels to enter the pipeline:
- `workbuddy` — opts the issue into the state machine
- `status:developing` — triggers the dev-agent

Missing either label = nothing happens. This is the #1 reason "nothing happens
after I create the issue."

```bash
gh issue create -R Owner/Repo \
  --title "Your title" \
  --body '...' \
  --label "workbuddy,status:developing"
```

## What makes a good Acceptance Criterion

Each criterion must be **individually verifiable** — the review-agent evaluates
them one by one as pass/fail/cannot-judge with concrete evidence.

### Good criteria (specific, verifiable)

```markdown
## Acceptance Criteria
- [ ] Function `calculateTotal()` in `src/billing.go` returns correct tax for all US states
- [ ] Unit test `TestCalculateTotal_StateTax` exists and passes
- [ ] `go test ./billing/... -cover` shows >80% coverage for `billing.go`
```

Why this works: each criterion points to something concrete (a function, a test
name, a measurable coverage number). The review-agent can check each one
independently.

### Bad criteria (vague, compound, subjective)

```markdown
## Acceptance Criteria
- [ ] The billing module works correctly
- [ ] Good test coverage
- [ ] Code is clean and follows best practices
```

Why this fails:
- "works correctly" — how does the review-agent verify this?
- "good test coverage" — what number? 50%? 80%? For which files?
- "clean and follows best practices" — subjective, cannot be pass/fail

### Common mistakes

**Compound criteria**: "Add validation AND update the API AND write tests"
- Split into 3 separate criteria. If one fails, the dev-agent needs to know which.

**Implementation-prescriptive**: "Use the Strategy pattern to refactor the payment module"
- Focus on WHAT should change, not HOW. Let the agent decide the approach.
- Better: "Payment module supports adding new payment providers without modifying existing code"

**Coverage confusion**: "Test coverage > 80% for the queue package"
- Clarify: does this mean `go test -cover ./queue/...` (package-level) or
  per-file coverage of specific files? Package-level includes ALL files,
  making the threshold much harder to hit if the package has many non-test files.
- Better: "Coverage of functions in `queue.go` exceeds 80% (measured by `go tool cover -func`)"

## Issue body structure

A complete issue body for workbuddy:

```markdown
## Description
What needs to be done and why. Give the agent enough context to understand
the codebase area and the motivation.

## Acceptance Criteria
- [ ] Criterion 1 (individually verifiable)
- [ ] Criterion 2
- [ ] Tests exist for the above

## Additional Context
- Related issues: #12, #34
- Relevant files: `src/module/file.go`
- Constraints: must not break existing API
```

The `## Description` section helps the agent understand context. Without it,
the agent only has the title and the criteria — often not enough for complex tasks.

## Heading format matters

The agent prompt looks for `## Acceptance Criteria` (h2 markdown heading).
These formats may cause issues:

| Format | Works? | Notes |
|--------|--------|-------|
| `## Acceptance Criteria` | Yes | Standard, always works |
| `### Acceptance Criteria` | Usually | Most agents handle h3 |
| `**Acceptance Criteria:**` | Risky | Bold text, not a heading — some agents miss it |
| `Acceptance Criteria` (plain) | No | Not parsed as a section |

Use `##` (h2) to be safe.

## Quick template

Copy-paste this for a new issue:

```bash
gh issue create -R Owner/Repo \
  --title "Brief description of the task" \
  --body '## Description
What needs to be done and why.

## Acceptance Criteria
- [ ] First verifiable criterion
- [ ] Second verifiable criterion  
- [ ] Tests exist and pass

## Additional Context
Any relevant links, files, or constraints.' \
  --label "workbuddy,status:developing"
```
