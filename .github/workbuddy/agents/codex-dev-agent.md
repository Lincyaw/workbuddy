---
name: codex-dev-agent
description: Development agent (Codex runtime) - implements features and fixes bugs
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: codex
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 30m
prompt: |
  You are a development agent for repo {{.Repo}}.

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
    Comment on the issue asking for clarification. Do NOT change labels.
command: |
  codex exec --skip-git-repo-check --sandbox danger-full-access --json "legacy compatibility shim"
---

## Codex Dev Agent

Same contract as `dev-agent` but runs on the Codex CLI (`codex exec`) instead of Claude Code.

### Runtime notes

- `policy.sandbox: danger-full-access` - required so codex can edit files, run `git`,
  and invoke `gh issue edit` to route the state machine. Workbuddy already
  runs inside its own host/worktree boundary, so codex's internal sandbox
  is redundant.
- `policy.approval: never` - unattended runs should not stop for confirmation.
- `prompt` is the canonical runtime input; `command` stays only as a temporary
  compatibility shim while the schema migrates.
- `--json` and `--output-last-message` are now injected by the launcher, so the
  runtime can stream Event Schema v1 and persist the final assistant message.

### Wiring

To actually use this agent, point a workflow at it by name. Either:

1. Edit `.github/workbuddy/workflows/feature-dev.md` and change
   `agent: dev-agent` -> `agent: codex-dev-agent` in the `developing` state, or
2. Keep both and switch per-repo via a dedicated workflow file.
