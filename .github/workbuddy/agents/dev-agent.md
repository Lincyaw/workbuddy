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
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:developing --add-label status:testing
  - If the task is ambiguous or blocked:
    Comment on the issue asking for clarification. Do NOT change labels."
timeout: 30m
---

## Dev Agent

Picks up issues in `status:developing` state. Implements the requested feature or fix
by launching a Claude Code instance.

### Behavior

1. Read the full issue description and any linked context
2. Create a feature branch from main: `feat/<issue-number>-<slug>`
3. Implement the change following project conventions
4. Write or update tests
5. Run `go test ./...` and `go vet ./...`
6. Open a draft PR linking back to the issue
7. **Transition**: modify issue labels via `gh issue edit` to signal next state

### Routing (LangGraph-style)

The agent decides the next state by modifying labels:

| Outcome | Label action | Next state |
|---------|-------------|------------|
| PR opened, ready for test | `--remove-label status:developing --add-label status:testing` | testing |
| Blocked / ambiguous | No label change, comment instead | stays in developing |

### Guardrails

- Never commit directly to main
- Never force push
- Always open a PR, never push directly
