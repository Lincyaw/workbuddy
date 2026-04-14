---
name: review-agent
description: Review agent - performs code review on PRs
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: claude-code
command: >
  claude -p "You are a code review agent for repo {{.Repo}}.

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
    Post a PR review with request-changes explaining what needs fixing."
timeout: 15m
---

## Review Agent

Performs automated code review when an issue enters the `reviewing` state.

### Routing

| Outcome | Label action | Next state |
|---------|-------------|------------|
| Approved | `status:reviewing → status:done` + close issue | done |
| Changes requested | `status:reviewing → status:developing` | developing (triggers retry count) |
