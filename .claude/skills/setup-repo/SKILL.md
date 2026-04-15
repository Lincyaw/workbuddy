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
status:reviewing — #D93F0B — Under review (reviewer runs tests + review)
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
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:developing --add-label status:reviewing
  - If the task is ambiguous or blocked:
    Comment on the issue asking for clarification. Do NOT change labels."
timeout: 30m
---
```

#### review-agent.md
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
  testing — run the test suite as a blocking gate before approving.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}

  ## Steps
  1. Check out the PR branch (gh pr checkout <N>)
  2. Run build + vet + tests as blocking gates (substitute for the repo's stack):
     go build ./... && go vet ./... && go test ./... -count=1
  3. Review code quality, tests, and adherence to project conventions
  4. If all checks pass AND code is approved:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:done
  5. If tests fail OR changes needed:
     Comment on the PR with failing output and feedback, then:
     Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:developing"
timeout: 30m
---
```

**Adapt the test commands** based on the project's language/stack (in review-agent):
- For Go projects: `go test ./...`, `go vet ./...`
- For Node.js projects: `npm test`, `npm run lint`
- For Python projects: `pytest`, `ruff check`

### Step 4: Create workflow definitions

Create `.github/workbuddy/workflows/` directory and workflow markdown files.

#### feature-dev.md (standard feature workflow)
````markdown
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
````

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
   triage -> developing <-> reviewing -> done
                 ^_______________|
                 (back-edges: retry tracked, max 3)
   Any back-edge exceeding max_retries -> failed -> needs-human
   (Reviewer owns testing — runs build/vet/tests as the blocking gate.)
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
