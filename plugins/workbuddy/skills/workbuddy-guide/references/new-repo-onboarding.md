# New Repository Onboarding Checklist

Step-by-step guide for configuring a new repository to work with workbuddy.
This is the practical checklist — the setup-repo skill automates most of this,
but understanding each step helps when debugging or doing manual setup.

## Prerequisites

Before starting, verify:

1. **`gh` CLI authenticated** with write access to the target repo
   ```bash
   gh auth status
   # Must show ✓ Logged in, with 'repo' scope
   ```

2. **Agent runtime installed** — at least one of:
   - `claude` CLI (for claude-code runtime)
   - `codex` CLI (for codex runtime)

3. **Git remote uses HTTPS** (not SSH) if `gh` is your auth mechanism
   ```bash
   git remote -v
   # If SSH: git remote set-url origin https://github.com/OWNER/REPO.git
   ```
   Why: `gh auth` configures HTTPS tokens. SSH uses separate keys and may
   map to a different GitHub account than `gh` is logged into.

4. **workbuddy binary built**
   ```bash
   cd /path/to/workbuddy && go build -o workbuddy .
   ```

## Step 1: Create GitHub Labels

These 5 labels are required. The `workbuddy` label opts an issue into the
state machine; the `status:*` labels drive transitions.

```bash
REPO="Owner/RepoName"
gh label create "workbuddy"          --color "5319E7" --description "Managed by workbuddy" -R $REPO 2>/dev/null || true
gh label create "status:developing"  --color "1D76DB" --description "Agent is developing"  -R $REPO 2>/dev/null || true
gh label create "status:reviewing"   --color "D93F0B" --description "Agent is reviewing"   -R $REPO 2>/dev/null || true
gh label create "status:done"        --color "0E8A16" --description "Task completed"       -R $REPO 2>/dev/null || true
gh label create "status:blocked"     --color "BFD4F2" --description "Needs human action"   -R $REPO 2>/dev/null || true
```

## Step 2: Create Config Directory

```bash
cd /path/to/target-repo
mkdir -p .github/workbuddy/agents .github/workbuddy/workflows
```

## Step 3: Write config.yaml

```yaml
# .github/workbuddy/config.yaml
environment: dev
repo: Owner/RepoName        # Must match GitHub's owner/repo exactly
poll_interval: 30s
port: 8090                   # Pick a unique port if running multiple instances
```

Important: The `repo` field must match the GitHub repo identifier exactly
(case-sensitive for the owner part on some API calls).

## Step 4: Write Agent Configs

Copy `dev-agent.md` and `review-agent.md` from the workbuddy repo's
`.github/workbuddy/agents/` directory. The prompts are generic and work
across any repo — they reference `{{.Repo}}`, `{{.Issue.Number}}`, etc.

Key fields to customize per repo:
- `runtime`: `claude-code` or `codex` — pick based on what's installed
- `policy.timeout`: increase for repos with slow builds (default: 60m dev, 15m review)

## Step 5: Write Workflow Definition

Copy `workflows/default.md` from the workbuddy repo. The default workflow
works for all repos — it's the 2-agent state machine with 3 retries.

## Step 6: Initialize Agent Runtime Skills

Agent runtimes (claude-code, codex) discover project-level skills from
conventional directories. Without this step, agents only have generic
global skills and miss repo-specific development patterns.

Create skills matching the runtime in your agent configs:

```bash
# For codex runtime:
mkdir -p .codex/skills/dev-loop .codex/skills/long-horizon

# For claude-code runtime:
mkdir -p .claude/skills/dev-loop .claude/skills/long-horizon
```

Each skill needs a `SKILL.md` with at minimum:

```yaml
---
name: dev-loop
description: "Run a complete development loop: implement, test, verify.
  Use when implementing features, fixing bugs, or refactoring."
---

# Dev Loop

## Build & Test Commands
- Build: <detect from project>
- Test: <detect from project>
- Lint: <detect from project>

## Workflow
implement → test → vibe-check → review → measure → keep or discard
```

For codex skills, also create `agents/openai.yaml` alongside each SKILL.md:

```yaml
interface:
  display_name: "Dev Loop"
  short_description: "Run a full implement-test-measure loop"
  default_prompt: "Use $dev-loop for development tasks."
```

Standard skills to include:
- **dev-loop** — development cycle with repo-specific build/test commands
- **long-horizon** — autonomous decision-making escalation ladder
- **index** (optional) — doc-code consistency, if the repo has docs

Why this matters: we confirmed via testing that codex discovers skills
from `<workdir>/.codex/skills/` based on the `--cd` flag. Without
project-level skills, agents rely only on the prompt template and
generic global skills (pdf, docx, etc.) which are irrelevant to dev work.

## Step 7: Add .workbuddy/ to .gitignore

**This is easy to forget and causes problems if missed.**

```bash
echo '.workbuddy/' >> .gitignore
```

Why: workbuddy creates `.workbuddy/` at runtime for SQLite DB and session
logs. Without this gitignore entry, agents may accidentally commit session
artifacts (100KB+ JSONL files) into the repo.

## Step 8: Commit and Push

```bash
git add .github/workbuddy/ .gitignore
git commit -m "chore: add workbuddy agent orchestration config"
git push origin main
```

Note: If the target repo uses conventional commits (enforced by lefthook or
similar), use the appropriate prefix (`chore:`, `ci:`, etc.).

## Step 9: Validate

```bash
cd /path/to/target-repo
/path/to/workbuddy validate
# Exit 0 = OK, any output = warnings/errors
```

## Step 10: Register with Coordinator (distributed mode only)

If running in distributed mode, register the repo with the coordinator. The
recommended auth input is either `WORKBUDDY_AUTH_TOKEN` in the env or
`--token-file /path/to/token`. Plain `--token <value>` still works but prints a
deprecation warning because it leaks into `ps`/shell history.

```bash
export WORKBUDDY_AUTH_TOKEN="your-token"
/path/to/workbuddy repo register \
  --coordinator http://coordinator-host:8081
```

This must be run from the target repo's root directory (where `.github/workbuddy/`
lives). The command serializes the local config and POSTs it to the coordinator.
Add `--format json` if you need machine-readable confirmation output.

## Step 11: Start a Worker (distributed mode only)

```bash
/path/to/workbuddy worker \
  --coordinator http://coordinator-host:8081 \
  --token-file /etc/workbuddy/auth-token \
  --runtime codex \
  --repos Owner/RepoName=/path/to/repo
```

Notes:
- `--repos OWNER/NAME=/path` is the canonical repo-binding flag. The legacy
  `--repo Owner/RepoName` alias still works but defaults the path to cwd and
  prints a deprecation warning.
- Use `--token-file` (or the `WORKBUDDY_AUTH_TOKEN` env var) instead of passing
  a secret on the command line.

The worker must be started from the target repo's directory because it loads
agent prompt templates from the local `.github/workbuddy/agents/` directory.

## Step 12: Create a Test Issue

```bash
gh issue create -R Owner/RepoName \
  --title "Test: verify workbuddy pipeline" \
  --body '## Description
Smoke test for workbuddy integration.

## Acceptance Criteria
- [ ] A file named `workbuddy-test.md` exists at repo root with content "hello workbuddy"
- [ ] The file is committed on a branch named `workbuddy/issue-<N>`' \
  --label "workbuddy,status:developing"
```

Watch the issue for agent comments. The full cycle should take 5-15 minutes
depending on agent runtime and task complexity.

## Verification Checklist

After the test issue completes (or after first dispatch), verify:

- [ ] Coordinator log shows `state entry detected: <repo>#<N> entered "developing"`
- [ ] Worker log shows `[codex-debug]` or `[claude-debug]` (agent subprocess started)
- [ ] Issue has "Agent Started" comment with session ID and worker ID
- [ ] Agent creates a branch, commits, pushes, and creates a PR
- [ ] Agent changes labels from `status:developing` to `status:reviewing`
- [ ] Review-agent picks up and evaluates acceptance criteria
- [ ] Final state is either `status:done` (pass) or retry cycle (fail → developing)
