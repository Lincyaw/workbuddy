---
name: setup-repo
description: "Configure a GitHub repository for workbuddy orchestration — creates labels, agent configs, workflow definitions, and config.yaml. Use when the user says 'set up repo', 'configure repo for workbuddy', 'initialize workbuddy', '配置仓库', '初始化workbuddy', or wants to onboard a new repo."
user_invocable: true
---

# Setup Repo

Interactive skill that configures a GitHub repository for workbuddy agent orchestration.
Creates all required labels, agent definitions, workflow state machines, and configuration files.

## When to use

- When onboarding a new repository for workbuddy
- When the user wants to add workbuddy support to an existing project
- When recreating or resetting workbuddy configuration

## Arguments

The skill accepts one optional argument: the GitHub repository identifier (e.g., `owner/repo`).
If not provided, prompt the user or detect from the current git remote.

## What to do

### Step 1: Gather information

Determine:
1. **Target repo** — from the argument, or detect via `gh repo view --json nameWithOwner -q .nameWithOwner`
2. **Project language/stack** — scan the repo for `go.mod`, `package.json`, `pyproject.toml`, `Cargo.toml`, etc.
3. **Existing labels** — run `gh label list --repo <repo> --json name -q '.[].name'`
4. **Existing workbuddy config** — check if `.github/workbuddy/` already exists

### Step 2: Create status labels

Workbuddy uses label-driven state machines. Create the required labels if they don't exist:

```
type:feature     — #0E8A16 — Feature request
type:bug         — #D73A4A — Bug report
type:task        — #0075CA — Generic task
status:triage    — #FBCA04 — Awaiting triage
status:developing — #1D76DB — Under development
status:testing   — #5319E7 — Under testing
status:reviewing — #D93F0B — Under review
status:done      — #0E8A16 — Completed
status:failed    — #B60205 — Failed, needs human intervention
needs-human      — #E99695 — Requires human intervention
```

Use `gh label create` for each label. Skip labels that already exist (check first).

### Step 3: Create agent definitions

Create `.github/workbuddy/agents/` directory and agent markdown files.
Each agent file has YAML frontmatter + descriptive markdown body.

**Required agents for a standard workflow:**

#### dev-agent.md
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
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:developing --add-label status:testing
  - If the task is ambiguous or blocked:
    Comment on the issue asking for clarification. Do NOT change labels."
timeout: 30m
---
```

#### test-agent.md
```yaml
---
name: test-agent
description: Testing agent - runs tests and validates implementation
triggers:
  - label: "status:testing"
    event: labeled
role: test
runtime: claude-code
command: >
  claude -p "You are a testing agent for repo {{.Repo}}.

  ## Task
  Review the code changes related to issue #{{.Issue.Number}} and run tests.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}

  ## Steps
  1. Find the PR associated with issue #{{.Issue.Number}}
  2. Review the code changes
  3. Run the project's test suite
  4. If tests pass and code looks correct:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:testing --add-label status:reviewing
  5. If tests fail or code has issues:
     Comment on the PR with specific feedback, then:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:testing --add-label status:developing"
timeout: 15m
---
```

#### review-agent.md
```yaml
---
name: review-agent
description: Review agent - performs code review
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: claude-code
command: >
  claude -p "You are a code review agent for repo {{.Repo}}.

  ## Task
  Review the PR associated with issue #{{.Issue.Number}}.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}

  ## Steps
  1. Find the PR for this issue
  2. Review code quality, tests, and adherence to project conventions
  3. If approved:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:done
  4. If changes needed:
     Comment on the PR with review feedback, then:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:developing"
timeout: 15m
---
```

**Adapt the command templates** based on the project's language/stack:
- For Go projects: include `go test ./...`, `go vet ./...` in test-agent
- For Node.js projects: include `npm test`, `npm run lint` in test-agent
- For Python projects: include `pytest`, `ruff check` in test-agent

### Step 4: Create workflow definitions

Create `.github/workbuddy/workflows/` directory and workflow markdown files.

#### feature-dev.md (standard feature workflow)
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
      - to: testing
        when: labeled "status:testing"

  testing:
    enter_label: "status:testing"
    agent: test-agent
    transitions:
      - to: reviewing
        when: labeled "status:reviewing"
      - to: developing
        when: labeled "status:developing"

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

#### bugfix.md (similar but for bugs)
Create a similar workflow with `trigger.issue_label: "type:bug"`.

### Step 5: Create config.yaml

Create `.github/workbuddy/config.yaml`:

```yaml
# Workbuddy configuration for <repo>
environment: dev
repo: <owner/repo>
poll_interval: 30s

server:
  port: 8080
  host: 0.0.0.0
```

### Step 6: Create .workbuddy/ gitignore entry

Add `.workbuddy/` to the repo's `.gitignore` if not already present.
This directory holds local runtime state (SQLite DB, session logs).

### Step 7: Verify and report

After creating all files:

1. List what was created:
   - Labels created (count)
   - Agent configs created (list names)
   - Workflows created (list names)
   - config.yaml location

2. Show the state machine diagram:
   ```
   triage -> developing <-> testing <-> reviewing -> done
                 ^_________________________|
                 (back-edges: retry tracked, max 3)
   Any back-edge exceeding max_retries -> failed -> needs-human
   ```

3. Explain how to use:
   - Start workbuddy: `workbuddy serve`
   - Create an issue with `type:feature` label
   - Move to `status:developing` to trigger the dev-agent
   - Agents will automatically transition through states
   - Monitor progress on the GitHub issue

## What NOT to do

- Don't overwrite existing agent/workflow configs without asking
- Don't delete existing labels
- Don't modify code in the target repo
- Don't start workbuddy — just configure it
- Don't assume the repo uses a specific language — detect it

## Customization

If the user specifies custom requirements:
- Custom agents (e.g., deploy-agent, docs-agent) — create additional agent .md files
- Custom workflows (e.g., hotfix workflow with fewer states) — create additional workflow .md files
- Custom labels — add them to the label creation step
- Custom runtimes (codex instead of claude-code) — set `runtime: codex` in agent configs
