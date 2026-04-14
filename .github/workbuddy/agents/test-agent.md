---
name: test-agent
description: Test agent - runs test suites and reports results
triggers:
  - label: "status:testing"
    event: labeled
role: test
runtime: claude-code
command: >
  claude -p "You are a test agent for repo {{.Repo}}.

  ## Task
  Run the full test suite for the PR linked to issue #{{.Issue.Number}}.
  Report results as a comment on the issue.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  PR: {{.PR.URL}}

  ## Steps
  1. Check out the PR branch
  2. Run the full test suite (go test ./... -v -count=1)
  3. Run go vet ./...
  4. Collect test results and coverage

  ## When done
  - If ALL tests pass:
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:testing --add-label status:reviewing
  - If ANY test fails:
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:testing --add-label status:developing
    Comment the failure details on the issue so the dev agent knows what to fix."
timeout: 15m
---

## Test Agent

Runs when an issue enters the `testing` state. Executes the test suite against
the PR branch and decides the next state based on results.

### Routing

| Outcome | Label action | Next state |
|---------|-------------|------------|
| All tests pass | `status:testing → status:reviewing` | reviewing |
| Any test fails | `status:testing → status:developing` | developing (triggers retry count) |
