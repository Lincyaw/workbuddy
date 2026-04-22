---
name: setup-repo
description: "Configure a GitHub repository for workbuddy orchestration — creates labels, the two canonical agent configs (dev + review), workflow definitions, issue templates, and config.yaml. Use when the user says 'set up repo', 'configure repo for workbuddy', 'initialize workbuddy', '配置仓库', '初始化workbuddy', or wants to onboard a new repo."
---

# Setup Repo

Interactive skill that configures a GitHub repository for workbuddy agent orchestration.
Creates all required labels, the two canonical agent definitions (dev + review), the
state-machine workflows, issue templates with a mandatory Acceptance Criteria section,
and the `.github/workbuddy/config.yaml`.

## The 2-agent model (read this first)

Workbuddy's catalog is deliberately minimal: only `dev-agent` and `review-agent`.
Everything the previous 9-agent catalog did — triage, docs-only work, dependency
bumps, release prep, security audits, alternate runtime variants — collapses into
this contract:

- **Humans write issues** with a mandatory `## Acceptance Criteria` section listing
  verifiable criteria. This replaces the old `triage-agent` — a good issue template
  is more reliable than an LLM reverse-engineering intent.
- **`dev-agent`** (triggers on `status:developing`): reads the issue; if the
  Acceptance Criteria section is missing it sets `status:blocked` and comments;
  otherwise it produces whatever artifact the criteria demand — code, docs,
  a dependency bump, a report, release notes — then flips to `status:reviewing`.
- **`review-agent`** (triggers on `status:reviewing`): evaluates each criterion
  against the artifact. All pass → `status:done`. Any fail → `status:developing`
  with criterion-by-criterion feedback.
- **Runtime (`claude-code` | `codex`)** is a field on the agent config, not a
  separate agent. Switch runtimes via repo-level override, not by duplicating agents.
- **Operational detail** (gh CLI recipes, PR reconciliation, dev-loop commands,
  branch conventions) lives in the TARGET repo's own `CLAUDE.md` and optional
  `.claude/skills/`. Workbuddy's agent prompts are short contracts, not manuals.
  See "Step 6" below.

See `docs/decisions/2026-04-15-agent-role-consolidation.md` in the workbuddy repo
for the first-principles rationale.

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
5. **Existing issue templates** — check if `.github/ISSUE_TEMPLATE/` already has entries
6. **Git remote protocol** — run `git remote -v`. If remote uses SSH (`git@github.com:...`)
   but `gh auth status` shows HTTPS protocol, switch remote to HTTPS:
   ```bash
   git remote set-url origin https://github.com/OWNER/REPO.git
   ```
   This prevents push failures when agents try to push branches.
7. **Commit hooks** — check for lefthook/husky/commitlint. If the repo enforces
   conventional commits, note this — agents need to use the right prefix format.

### Step 2: Create labels

Workbuddy uses label-driven state machines. Create the required labels if they don't exist:

```
workbuddy         — #5319E7 — Opt-in: this issue flows through the workbuddy state machine
type:feature      — #0E8A16 — Feature request (optional classification)
type:bug          — #D73A4A — Bug report (optional classification)
status:developing — #1D76DB — Dev-agent is producing an artifact
status:reviewing  — #D93F0B — Review-agent is verifying acceptance criteria
status:blocked    — #E99695 — Needs human action (typically: missing Acceptance Criteria)
status:done       — #0E8A16 — All acceptance criteria verified green
```

The `workbuddy` label is the workflow trigger. Only issues carrying it
enter the state machine; plain GitHub issues without it are ignored. This
lets a team opt-in issue-by-issue rather than gating every new issue.

Notes on what was dropped from prior versions and why:
- `status:triage` — removed. Humans classify via issue template + type labels at
  creation time. A triage state machine node added latency without adding signal.
- `status:failed` + `needs-human` — merged into `status:blocked`. A single
  "waiting for human" state is simpler and covers retry-exhaustion,
  missing criteria, and dev-agent self-blocks uniformly.
- `type:task` — removed. Every issue is either a `type:feature` or `type:bug`
  (or the target repo can add its own custom type labels via Step 9 "Customization").

Use `gh label create` for each label. Skip labels that already exist (check first).

### Step 3: Create issue templates with Acceptance Criteria

Create `.github/ISSUE_TEMPLATE/feature.md` and `.github/ISSUE_TEMPLATE/bug.md`.
Both MUST include a `## Acceptance Criteria` section — this is the contract
`dev-agent` reads and `review-agent` verifies against.

#### feature.md
```markdown
---
name: Feature
about: Propose a new feature or enhancement
labels: ["workbuddy", "type:feature", "status:developing"]
---

## Context
<!-- What motivates this feature? What problem does it solve? -->

## Proposed Change
<!-- Describe what should be built. -->

## Acceptance Criteria
<!--
Each criterion must be individually verifiable. dev-agent produces an artifact
that satisfies every one; review-agent evaluates each as pass / fail / cannot-judge.
If this section is empty or missing, dev-agent will set status:blocked.
-->
- [ ]
- [ ]
- [ ]

## Additional Notes
<!-- Links, references, related issues. -->
```

#### bug.md
```markdown
---
name: Bug Report
about: Report a defect
labels: ["type:bug", "status:developing"]
---

## Observed Behavior
<!-- What actually happens? Include reproduction steps. -->

## Expected Behavior
<!-- What should happen instead? -->

## Acceptance Criteria
<!--
Typically: "the reproduction steps above no longer produce the observed behavior"
plus any regression-test criterion. Each item must be individually verifiable.
-->
- [ ] Reproduction steps no longer reproduce the bug on <platform/version>
- [ ] Regression test added at <location>
- [ ]

## Environment
<!-- OS, versions, config. -->
```

### Step 4: Create agent definitions

Create `.github/workbuddy/agents/` and write these two files — and only these two.
Do not create `triage-agent`, `docs-agent`, `dependency-bump-agent`, `release-agent`,
`security-audit-agent`, or `codex-*` duplicates. Do not create a `schemas/` directory —
the 2-agent catalog does not use `output_contract`.

#### dev-agent.md
```markdown
---
name: dev-agent
description: Development agent — produces an artifact satisfying the issue's Acceptance Criteria
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: claude-code
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 30m
prompt: |
  You are the dev agent for repo {{.Repo}}, working on issue #{{.Issue.Number}}.

  Read the issue body and look for a `## Acceptance Criteria` section.

  - If the section is missing or empty: remove label `status:developing`,
    add label `status:blocked`, and post a comment explaining that the issue
    needs verifiable acceptance criteria before dev work can start. Stop.
  - Otherwise: produce the artifact that satisfies every criterion. The
    artifact form depends on the criteria themselves — it may be code, docs,
    a dependency bump, a report, release notes, or any combination. Include
    tests or automated checks for every verifiable criterion.

  When the artifact is linked to this issue, remove label `status:developing`
  and add label `status:reviewing`.

  Use this repo's own CLAUDE.md / .claude/skills/ for project-specific
  dev-loop commands, PR conventions, and tooling. Report the artifact link
  when finished.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  Body:
  {{.Issue.Body}}
timeout: 30m
---

## Dev Agent

Consumes the issue's `## Acceptance Criteria` and produces an artifact that
satisfies every criterion. Transitions to `status:blocked` if criteria are
missing; otherwise transitions to `status:reviewing` when done.
```

#### review-agent.md
```markdown
---
name: review-agent
description: Review agent — verifies the artifact against the issue's Acceptance Criteria
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: claude-code
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 15m
prompt: |
  You are the review agent for repo {{.Repo}}, verifying the artifact
  produced for issue #{{.Issue.Number}}.

  Read the issue's `## Acceptance Criteria` section and the artifact
  (PR, comment, or file) linked to this issue. For each criterion, evaluate
  it as pass / fail / cannot-judge, citing concrete evidence — file:line,
  test name, or a quoted excerpt.

  - If every criterion passes: remove label `status:reviewing`, add label
    `status:done`, and post a comment with the criterion-by-criterion verdict.
  - If any criterion fails: remove label `status:reviewing`, add label
    `status:developing`, and post a comment listing the failing criteria
    plus what dev needs to address.

  Use this repo's own CLAUDE.md / .claude/skills/ for project-specific
  review conventions.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  Body:
  {{.Issue.Body}}
timeout: 15m
---

## Review Agent

Evaluates each acceptance criterion against the produced artifact. All pass →
`status:done`. Any fail → `status:developing` with feedback.
```

### Step 5: Create the workflow definition

Create `.github/workbuddy/workflows/default.md`. Exactly one workflow per
repo — bug vs feature use the same state machine, so they share this file.
The classification (`type:feature` / `type:bug`) is a labeling convention
for humans browsing issues; it does not alter the execution path.

````markdown
---
name: default
description: Default 2-agent lifecycle for any workbuddy-tracked issue
trigger:
  issue_label: "workbuddy"
max_retries: 3
---

## Default Workflow

Every issue labeled `workbuddy` flows through this state machine.

```yaml
states:
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: reviewing
        when: labeled "status:reviewing"
      - to: blocked
        when: labeled "status:blocked"

  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      - to: done
        when: labeled "status:done"
      - to: developing
        when: labeled "status:developing"

  blocked:
    enter_label: "status:blocked"
    transitions:
      - to: developing
        when: labeled "status:developing"

  done:
    enter_label: "status:done"
```

### State graph

```
developing ⇄ reviewing → done
   ↑             ↓
   └─── blocked ─┘  (human edits issue, flips back to developing)

Any revisit of a state — including developing↔blocked — counts toward
max_retries; exceeding the limit records retry/failure intent. Label
writeback remains an agent/human action, and closing the issue after merge is
still the responsibility of the review-agent or the human who merged the PR.
```
````

### Step 6: Operational detail lives in the target repo

Workbuddy's agent prompts are intentionally minimal contracts. They do NOT
encode gh CLI recipes, branch-switching commands, PR reconciliation rules,
or dev-loop commands — because those differ per repo and are better maintained
where they are used.

Recommend to the user: the target repo should maintain its own `CLAUDE.md`
describing:
- Dev-loop commands (build, test, vet, lint) for the repo's stack
- PR conventions (branch naming, draft vs ready, review gating, merge strategy)
- Test strategy (mock/real boundary, fixtures, coverage targets)
- Project-specific gh CLI patterns

If the target repo has no `CLAUDE.md`, offer to scaffold one — but only
after confirming with the user, since that's beyond "workbuddy setup".

### Step 6b: Initialize agent runtime skills in the target repo

Agent runtimes discover project-level skills from conventional directories
inside the working directory. **This step is critical** — without it, agents
only have generic global skills and miss repo-specific development patterns.

The skill directories depend on the runtime configured in the agent definitions:

| Runtime | Skill directory | Discovery |
|---------|----------------|-----------|
| `claude-code` | `.claude/skills/<name>/SKILL.md` | Auto-loaded when skill description matches context |
| `codex` | `.codex/skills/<name>/SKILL.md` | Same — auto-loaded by description match |

**Create skills for the runtime(s) used by this repo's agents.** If
`runtime: codex` is set in the agent configs, create `.codex/skills/`. If
`runtime: claude-code`, create `.claude/skills/`. If both runtimes are used
(e.g., different agents use different runtimes), create both directories.

#### Standard skills to initialize

Create these three skills — they are runtime-agnostic in content and give
agents a solid operational foundation:

**1. `dev-loop/SKILL.md`** — Development cycle: implement → test → verify

```yaml
---
name: dev-loop
description: "Run a complete development loop: implement, test, vibe-check,
  review, and measure before keeping or discarding. Use when implementing
  features, fixing bugs, writing code, or refactoring."
---
```

Body should describe the repo's specific build/test/lint commands, the
keep/discard decision framework, and how to log progress. Detect the
repo's language/stack (from Step 1) and tailor the commands accordingly.
For example, a Go repo should mention `go build`, `go test`, `go vet`;
a Node repo should mention `npm test`, `npm run lint`, etc.

**2. `long-horizon/SKILL.md`** — Autonomous decision-making

```yaml
---
name: long-horizon
description: "Framework for autonomous decision-making during extended tasks.
  Use when working on complex multi-step tasks that require judgment calls
  without human intervention."
---
```

Body should describe the escalation ladder (convention check → codebase
research → external research → north-star reasoning → ask human) and
decision logging conventions. This helps agents make good autonomous
decisions during dev and review cycles instead of getting stuck.

**3. `index/SKILL.md`** (optional, for repos with documentation) — Doc-code consistency

Only create this if the repo has existing documentation (`docs/`, `README.md`,
design docs, etc.). It helps agents keep documentation aligned with code changes.

#### Codex-specific: agent metadata files

For codex skills, also create an `agents/openai.yaml` alongside each
`SKILL.md` with display metadata:

```yaml
# .codex/skills/dev-loop/agents/openai.yaml
interface:
  display_name: "Dev Loop"
  short_description: "Run a full implement-test-measure loop"
  default_prompt: "Use $dev-loop to drive this task through implementation,
    testing, and verification."
```

#### What to put in the skill body

Keep skills **lean and repo-specific**:
- Reference the repo's actual build/test commands (detected in Step 1)
- Reference the repo's CLAUDE.md for conventions
- Do NOT duplicate generic methodology — link to it or summarize briefly
- The goal is that when an agent reads the skill, it knows exactly how
  to operate in THIS repo, not just in theory

#### Commit the skills

Add the skills directory to the same commit as the workbuddy config:

```bash
git add .codex/skills/   # or .claude/skills/ depending on runtime
```

The skills are version-controlled alongside the repo's code, so they
evolve with the project.

### Step 7: Create config.yaml

Create `.github/workbuddy/config.yaml`:

```yaml
# Workbuddy configuration for <repo>
environment: dev
repo: <owner/repo>
poll_interval: 30s
port: 8080
```

### Step 8: Create .workbuddy/ gitignore entry

Add `.workbuddy/` to the repo's `.gitignore` if not already present.
This directory holds local runtime state (SQLite DB, session logs).

### Step 9: Verify and report

After creating all files:

1. List what was created:
   - Labels created (count)
   - Agent configs created: dev-agent, review-agent (exactly 2)
   - Issue templates created: feature.md, bug.md
   - Workflow created: default (single state machine for all workbuddy issues)
   - config.yaml location

2. Show the state machine diagram:
   ```
   developing ⇄ reviewing → done
      ↑             ↓
      └─── blocked ─┘  (human edits issue, flips back to developing)

   Reviewer evaluates each Acceptance Criterion from the issue body.
   All pass → done. Any fail → back to developing. Max 3 retries.
   ```

3. Explain how to use:
   - Start workbuddy: `workbuddy serve`
   - Open an issue via the feature or bug template; fill the Acceptance Criteria
   - The `workbuddy` label opts the issue into the state machine; the
     `status:developing` label on creation triggers dev-agent automatically
   - Labels flow: developing → reviewing → done (or blocked when criteria missing)
   - Monitor progress on the GitHub issue

### Step 10: Register with coordinator (distributed mode)

If the user is running workbuddy in distributed mode, register the repo
after committing and pushing the config:

```bash
export WORKBUDDY_AUTH_TOKEN="<token>"
workbuddy repo register \
  --coordinator http://coordinator-host:8081 \
  --token "$WORKBUDDY_AUTH_TOKEN"
```

This must be run from the target repo's root directory. It serializes
the local `.github/workbuddy/` config and POSTs it to the coordinator.

Then start a worker for the repo:

```bash
workbuddy worker \
  --coordinator http://coordinator-host:8081 \
  --token "$WORKBUDDY_AUTH_TOKEN" \
  --runtime codex \
  --repo Owner/Repo
```

The worker must also be started from the repo root — it loads agent
prompt templates from `.github/workbuddy/agents/` locally.

## What NOT to do

- Don't create agents beyond `dev-agent` and `review-agent`. If a user insists,
  point them at the 2-agent decision record and suggest encoding the need as
  an issue template + acceptance criteria instead.
- Don't create `.github/workbuddy/agents/schemas/` — the 2-agent catalog has
  no `output_contract`.
- Don't re-introduce `status:triage`, `status:failed`, or `needs-human` labels —
  use `status:blocked` for all human-handoff cases.
- Don't overwrite existing agent/workflow configs without asking.
- Don't delete existing labels.
- Don't modify code in the target repo.
- Don't start workbuddy — just configure it.
- Don't assume the repo uses a specific language — detect it.

## Customization

If the user specifies custom requirements:
- **Custom runtime** (`codex` instead of `claude-code`) — set `runtime: codex`
  on the agent config. Runtime is a field, not a separate agent.
- **Custom type labels** beyond feature/bug — add the label and a matching
  issue template with the same `## Acceptance Criteria` contract. The
  workflow still matches on `workbuddy`, so the type label is classification
  only. If you genuinely need a different state machine for a particular
  class of work, add a new workflow file with a distinct trigger label and
  have the team apply that label explicitly.
- **Custom acceptance criteria conventions** — the only hard contract is that
  dev-agent reads a section literally titled `## Acceptance Criteria`. How the
  team structures criteria within that section is up to the repo.
- **Custom policy per label** (e.g. read-only sandbox for high-risk changes) —
  this is planned as Coordinator-side dynamic dispatch keyed on issue labels.
  Not yet implemented; for now every issue runs under the agent's default policy.
