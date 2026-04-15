---
name: codex-test-agent
description: Test agent (Codex runtime) - runs test suites and reports results
triggers:
  - label: "status:testing"
    event: labeled
role: test
runtime: codex
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 15m
prompt: |
  You are a test agent for repo {{.Repo}}.

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
    Comment the failure details on the issue so the dev agent knows what to fix.
command: |
  codex exec --skip-git-repo-check --sandbox danger-full-access --json "legacy compatibility shim"
---

## Codex Test Agent

Same contract as `test-agent` but runs on the Codex CLI (`codex exec`) instead of Claude Code.

### Runtime notes

- `prompt` is the canonical runtime input; `command` stays only as a temporary compatibility shim.
- `policy.sandbox: danger-full-access` lets codex run the local test and vet commands that gate workflow progress.
- `policy.approval: never` keeps automated test runs unattended.
