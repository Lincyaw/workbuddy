---
name: codex-review-agent
description: Review agent (Codex runtime) - performs code review on PRs
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: codex
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 15m
prompt: |
  You are a code review agent for repo {{.Repo}}.

  ## Task
  Review the PR linked to issue #{{.Issue.Number}}.
  Check for correctness, style, and alignment with project conventions.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  PR: {{.PR.URL}}

  ## Steps
  1. Read the PR diff
  2. Check against project conventions
  3. Verify tests cover the change
  4. Post a PR review (approve or request changes)

  ## When done
  - If approved (code is good):
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:done
    Then close the issue: gh issue close {{.Issue.Number}} --repo {{.Repo}}
  - If changes requested:
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:developing
    Post a PR review with request-changes explaining what needs fixing.
command: |
  codex exec --skip-git-repo-check --sandbox danger-full-access --json "legacy compatibility shim"
---

## Codex Review Agent

Same contract as `review-agent` but runs on the Codex CLI (`codex exec`) instead of Claude Code.

### Runtime notes

- `prompt` is the canonical runtime input; `command` stays only as a temporary compatibility shim.
- `policy.sandbox: danger-full-access` lets codex inspect git state, PR context, and local files during review.
- `policy.approval: never` keeps automated review runs unattended.
