# Workbuddy setup templates

Use this file only when the skill needs the concrete templates or exact label set.

## Required labels

```text
type:feature      #0E8A16  Feature request
type:bug          #D73A4A  Bug report
type:task         #0075CA  Generic task
status:triage     #FBCA04  Awaiting triage
status:developing #1D76DB  Under development
status:reviewing  #D93F0B  Under review (reviewer runs tests + review)
status:done       #0E8A16  Completed
status:failed     #B60205  Failed, needs human intervention
needs-human       #E99695  Requires human intervention
```

Create only the labels that do not already exist.

## Agent starter templates

### dev-agent.md

```yaml
---
name: dev-agent
description: Development agent - implements features and fixes bugs
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: claude-code
command: >
  claude -p "You are a development agent for repo {{.Repo}}.

  ## Task
  Read the issue below and implement the requested change.
  Create a feature branch, write code with tests, and open a draft PR.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  Body:
  {{.Issue.Body}}

  ## When done
  - If implementation is complete and PR is opened:
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:developing --add-label status:reviewing
  - If the task is ambiguous or blocked:
    Comment on the issue asking for clarification. Do NOT change labels."
timeout: 30m
---
```

### review-agent.md

```yaml
---
name: review-agent
description: Review agent - runs tests then reviews the PR
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: claude-code
command: >
  claude -p "You are a code review agent for repo {{.Repo}}.

  ## Task
  Review the PR associated with issue #{{.Issue.Number}}. Reviewer owns
  testing — run the project's test suite as a blocking gate before approving.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}

  ## Steps
  1. Find and check out the PR for this issue (gh pr checkout <N>)
  2. Run build + test + vet (substitute the repo's stack):
     go build ./... && go vet ./... && go test ./... -count=1
     If any of these fail, do NOT approve.
  3. Review code quality, tests, and adherence to project conventions
  4. If all checks pass AND code is approved:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:done
  5. If tests fail OR changes needed:
     Comment on the PR with failing output and review feedback, then:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:developing"
timeout: 30m
---
```

## Stack-specific test hints

Adjust the review agent's test command to match the repo:
- Go: include `go test ./...` and usually `go vet ./...`
- Node.js: include `npm test` and optionally `npm run lint`
- Python: include `pytest` and optionally `ruff check`
- Rust: include `cargo test`

If the repo already has a preferred validation command, use that instead of forcing a generic one.

## Workflow starter template

### feature-dev.md

```yaml
---
name: feature-dev
description: Full feature development lifecycle
trigger:
  issue_label: "type:feature"
max_retries: 3
---

## Feature Development Workflow

```yaml
states:
  triage:
    enter_label: "status:triage"
    transitions:
      - to: developing
        when: labeled "status:developing"

  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: reviewing
        when: labeled "status:reviewing"

  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      - to: done
        when: labeled "status:done"
      - to: developing
        when: labeled "status:developing"

  done:
    enter_label: "status:done"
    action: close_issue

  failed:
    enter_label: "status:failed"
    action: add_label "needs-human"
```
```

Create a similar workflow for bugs with `trigger.issue_label: "type:bug"`.

## config.yaml skeleton

```yaml
environment: dev
repo: <owner/repo>
poll_interval: 30s

server:
  port: 8080
  host: 0.0.0.0
```

## State machine summary

```text
triage -> developing <-> reviewing -> done
              ^_____________|
(back-edges are retries; exceed max_retries -> failed -> needs-human)
(reviewer runs the test suite itself; no separate testing state)
```
