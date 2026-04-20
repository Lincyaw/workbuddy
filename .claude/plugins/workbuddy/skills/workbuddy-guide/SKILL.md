---
name: workbuddy-guide
description: "Explain and operate workbuddy: what it is, which command to reach for, and where to find deeper references. **Trigger whenever the user mentions 'workbuddy' in any context** — including operational requests (register/onboard a repo, add/bind a worker, 注册仓库, 加仓库, 接入新仓库, onboard repo, set up worker), explanatory questions (how to use workbuddy, workbuddy guide, teach me workbuddy, 怎么用workbuddy, 使用指南), and deployment/running/operating/debugging workbuddy. Invoke this skill before running workbuddy CLI commands so the documented onboarding flow (coordinator register + worker repos add) isn't missed."
user_invocable: true
---

# Workbuddy Guide

A map of what workbuddy is and which command solves which problem. This
skill is intentionally terse — **for command details always run
`workbuddy <cmd> --help`**. The CLI help is the authoritative reference
and stays in sync with the code; duplicating it here would only drift.

## Bundled references (read when relevant)

- `references/new-repo-onboarding.md` — step-by-step checklist for adding a
  new repo to a workbuddy deployment. Read when setting up a repo.
- `references/known-pitfalls.md` — real-world failure modes and fixes. Read
  when something misbehaves.
- `references/writing-good-issues.md` — how to write issues that agents can
  actually process. Read when issues keep failing or getting blocked.

## What workbuddy is

GitHub Issue-driven agent orchestration. Humans file issues with
`## Acceptance Criteria` and the `workbuddy`+`status:developing` labels;
workbuddy polls GitHub, dispatches agents (Claude Code or Codex), and
reacts to label changes the agents make as they hand off work.

Only two agent roles exist: **`dev-agent`** and **`review-agent`**.
Runtime (`claude-code` | `codex`) is a config field, not a separate agent.

```
developing ⇄ reviewing → done
   ↕
 blocked  (missing Acceptance Criteria — human fixes, flips back)
```

Dev writes code + PR → flips to `reviewing`. Review evaluates each AC →
`done` on pass, `developing` on fail (max 3 retries via REQ-055 cap).

## Deployment modes — pick one

| Mode | Command | When to use |
|------|---------|-------------|
| **Serve** (single process) | `workbuddy serve` | Local dev, single-host setups, testing |
| **Distributed** (coordinator + workers) | `workbuddy coordinator` + `workbuddy worker` | Workers on different hosts, horizontal scale, multi-repo |
| **Managed install** (systemd) | `workbuddy deploy install` | Long-lived production; survives reboots; `deploy redeploy`/`deploy upgrade` for updates |

All three share the same state machine, SQLite store, and agent configs.
Run `workbuddy <mode> --help` for flags, `workbuddy deploy install --help`
for the systemd wrapper examples, etc.

## Command map — what to reach for

Run `workbuddy --help` for the full list. Grouped by intent:

**Setup a repo**
- `workbuddy init` — scaffold `.github/workbuddy/` in a new repo
- `workbuddy validate` — sanity-check config before running
- `workbuddy repo register` — attach a repo to a running coordinator (no restart)

**Run workbuddy**
- `workbuddy serve` — single-process dev mode
- `workbuddy coordinator` + `workbuddy worker` — distributed mode
- `workbuddy deploy install|redeploy|upgrade` — managed systemd install

**Observe**
- `workbuddy status` — issues, tasks, events, stuck issues, or watch until done
- `workbuddy logs <issue>` — per-attempt session logs (stdout/stderr/tool calls)
- `workbuddy diagnose` — surfaces stuck issues, 3-retry caps, stale claims; `--fix` for safe auto-remediation

**Recover**
- `workbuddy cache-invalidate` — force re-poll after manual label edits
- `workbuddy recover` — clean stale processes/worktrees/claims after a crash
- `workbuddy operator-watch` — auto-dispatch Claude on coordinator incident files

**Worker runtime ops**
- `workbuddy worker repos add|list|remove` — change a running worker's repo bindings without restart
- `workbuddy coordinator token create|list|revoke` — per-worker auth tokens

## Retry and failure semantics (worth knowing)

The state machine has several guardrails that change how failures look —
read the relevant decision docs for depth; the short version:

- **3-retry cap (REQ-055)**: after 3 consecutive agent failures on the same
  issue, dispatch stops. `workbuddy diagnose` surfaces this explicitly.
- **Infra vs verdict (REQ-056)**: launcher/runtime crashes are reported with
  an "Infra Error" comment header and do **not** burn the retry budget. Only
  agent-decided FAIL verdicts count toward the cap.
- **Issue-claim with TTL (REQ-057, hardened in REQ-059)**: a SQLite claim
  prevents two workers from dispatching the same issue concurrently. Stale
  claims self-heal after TTL expiry.
- **Worktree isolation (REQ-058)**: every task runs in its own
  `.workbuddy/worktrees/issue-N/`. If worktree setup fails, the worker
  reports loudly instead of falling back to CWD.

Impact for operators: if `diagnose` says "failed 3 times in a row", check
the issue's comments — if they're "Infra Error", the bug is infrastructure
(usually runtime/launcher), not the acceptance criteria. Fix infra and
`cache-invalidate` to restart; don't rewrite the issue.

## Common workflows

**Create a workbuddy issue**

```bash
gh issue create -R owner/name \
  --title "…" \
  --body '## Description
…

## Acceptance Criteria
- [ ] …' \
  --label "workbuddy,status:developing"
```

**Unstick an issue**

```bash
# Check what's wrong
workbuddy diagnose --repo owner/name

# Manually nudge state and force re-poll
gh issue edit N -R owner/name --remove-label status:blocked --add-label status:developing
workbuddy cache-invalidate --repo owner/name --issue N
```

**Add a repo to a running deployment** (no restart)

```bash
cd /path/to/new-repo
workbuddy repo register --coordinator http://coord:8081
# For a worker already running:
workbuddy worker repos add owner/new-repo=/path/to/new-repo
```

## Environment variables

`workbuddy <cmd> --help` lists the flags each command honors. Globals that
matter across commands:

| Variable | Purpose |
|----------|---------|
| `WORKBUDDY_AUTH_TOKEN` | Bearer token for coordinator auth (used with `--auth`) |
| `WORKBUDDY_SLACK_WEBHOOK_URL` / `WORKBUDDY_FEISHU_WEBHOOK_URL` / `WORKBUDDY_TELEGRAM_*` | Optional notification sinks |
| `WORKBUDDY_SMTP_*` | Optional email notifications |

## When you need more detail

1. `workbuddy <cmd> --help` — flags + examples (authoritative)
2. `references/` in this skill — setup, pitfalls, issue-writing
3. `docs/decisions/` and `docs/implemented/` in the repo — architecture rationale
4. `project-index.yaml` — requirements map (REQ-XXX IDs referenced above)

## Related skills

- `/setup-repo` — fully onboard a new repo (labels + config + validation)
- `/pipeline-monitor` — watch a running pipeline, diagnose stuck issues
- `/merge-flow` — batch-merge approved workbuddy PRs with conflict handling
