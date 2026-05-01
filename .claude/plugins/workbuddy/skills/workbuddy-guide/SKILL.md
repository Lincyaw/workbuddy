---
name: workbuddy-guide
description: "Explain and operate workbuddy: what it is, which command to reach for, and where to find deeper references. **Trigger whenever the user mentions 'workbuddy' in any context** — including operational requests (register/onboard a repo, add/bind a worker, 注册仓库, 加仓库, 接入新仓库, onboard repo, set up worker), explanatory questions (how to use workbuddy, workbuddy guide, teach me workbuddy, 怎么用workbuddy, 使用指南), deployment/running/operating/debugging workbuddy, and questions about how issues/tasks are scheduled or how dependencies between issues work (depends_on, blocked, dependency gate, 依赖, 调度, 排队, '为什么没派发'), and questions about parallel-development workflows (rollouts:N fan-out, synthesize/synthesis reduce step, cherry-pick, '并行开发', '多个方案', 'rollout'). Invoke this skill before running workbuddy CLI commands so the documented onboarding flow (coordinator register + worker repos add) and the dependency/scheduling model aren't re-derived from scratch."
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
| **Bundle** (default since v0.5: supervisor + coordinator + worker) | `workbuddy supervisor` + `workbuddy coordinator` + `workbuddy worker` | Production, multi-host, anywhere agent runs must survive worker restarts |
| **Serve** (single process, legacy) | `workbuddy serve` | Local dev, single-host quick-start; does **not** preserve agents across restart |
| **Managed install** (systemd) | `workbuddy deploy install` | Long-lived production; defaults to the bundle layout; `deploy redeploy`/`deploy upgrade`/`deploy rollback`/`deploy watch` for updates |

All three share the same state machine, SQLite store, and agent configs.
Run `workbuddy <mode> --help` for flags, `workbuddy deploy install --help`
for the systemd wrapper examples. See the `/deploy` skill for the bundle
topology rationale and the cutover from `serve`.

## Command map — what to reach for

Run `workbuddy --help` for the full list. Grouped by intent:

**Setup a repo**
- `workbuddy init` — scaffold `.github/workbuddy/` in a new repo
- `workbuddy validate` — sanity-check config before running
- `workbuddy repo register` — attach a repo to a running coordinator (no restart)

**Run workbuddy**
- `workbuddy supervisor` + `workbuddy coordinator` + `workbuddy worker` — bundle mode (default)
- `workbuddy serve` — single-process legacy mode (local dev only)
- `workbuddy deploy install|redeploy|upgrade|rollback|watch` — managed systemd install

**Observe**
- `workbuddy status` — issues, tasks, events, stuck issues, or `--watch` until done
- `workbuddy logs <issue>` — per-attempt session logs (stdout/stderr/tool calls)
- `workbuddy diagnose` — surfaces stuck issues, 3-retry caps, stale claims; `--fix` for safe auto-remediation
- `workbuddy hooks list|status|test|reload` — operator-owned event hooks (Slack/Feishu/etc.)

**Recover**
- `workbuddy cache invalidate` — force re-poll after manual label edits (`cache-invalidate` remains as a deprecated alias)
- `workbuddy issue restart` — clear poller cache + claim lease for one issue (was `admin restart-issue`, now deprecated)
- `workbuddy recover` — clean stale processes/worktrees/claims after a crash (`--kill-zombies --prune-worktrees --prune-remote-branches --reset-db`)
- `workbuddy operator-watch` — tail the operator incident inbox and auto-dispatch Claude per incident

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
`workbuddy cache invalidate` to restart; don't rewrite the issue.

## Dependencies & scheduling

Workbuddy has no separate "scheduler" or DAG runner — scheduling falls
out of three pieces working together. Knowing them avoids re-deriving
this every time:

1. **Polling.** The Coordinator polls each registered repo at
   `poll_interval` (config.yaml). GitHub label changes drive the state
   machine; nothing happens between polls. Force one with
   `workbuddy cache invalidate --repo owner/name --issue N`.
2. **Task queue + claim lease.** Eligible issues go into SQLite
   `task_queue`. Workers long-poll the Coordinator and claim by
   `claim_token` with a TTL, so two workers can never run the same task
   concurrently (REQ-057/059). Parallelism is per-worker per-repo —
   bind multiple workers (or `--repos owner/A=...,owner/B=...`) to fan
   out across repos.
3. **Dependency gate.** Declared in the issue **body**, not via labels:

   ````markdown
   ## Dependencies

   ```yaml
   workbuddy:
     depends_on:
       - "#26"
       - "Lincyaw/workbuddy#27"
     ```
   ````

   The Coordinator parses this every poll, computes a verdict
   (`ready` / `blocked` / `override` / `needs_human`), and refuses
   dispatch when blocked, logging a `dispatch_blocked_by_dependency`
   event. UX signal on GitHub: a single 😕 reaction added when blocked,
   removed on unblock. **No managed comment, no resolver agent** — the
   gate lives entirely in Coordinator Go.

   - Same-repo shorthand `#26` is normalized to `owner/repo#26`.
   - `owner/repo#N` cross-repo refs are accepted but unsupported in v0.1
     (verdict goes to `needs_human`).
   - Cycles, malformed refs, or "trigger label present + depends_on
     declared but no `status:*` label" all surface in `workbuddy diagnose`.
   - Human override: add the `override:force-unblock` label — verdict
     flips to `override` (treated as ready). Don't toggle `status:blocked`
     by hand; the Coordinator owns it.

   To see the verdict for one issue:
   ```bash
   workbuddy status --repo owner/name | grep "  N "      # CYCLES + DEPENDENCY columns
   workbuddy status --events --type dispatch_blocked_by_dependency --since 1h
   ```

   To re-evaluate after editing a dependency declaration:
   ```bash
   workbuddy cache invalidate --repo owner/name --issue N
   ```

There is no priority queue, no cron, no time-based scheduling. If you
want issue B to wait for issue A, declare it. If you want N tasks in
parallel, run N workers (or one worker bound to N repos).

## Parallel rollouts + synthesize (per-issue fan-out)

Beyond cross-issue parallelism, **a single issue** can also be developed
N ways in parallel and then reduced to one PR. This is opt-in per issue.

Mental model:

```
  status:developing  ──fan-out──►  N parallel dev-agent runs
       (rollouts:N label)            each on its own branch
                                     workbuddy/issue-<N>/rollout-K
                                     each opens its own PR labeled
                                     rollout:K with "[rollout K/N]" title
                                              │
                          (≥ join.min_successes succeed)
                                              ▼
                                    status:synthesizing
                                              │
                                  review-agent runs in mode: synthesize
                                  → reads all sibling PRs/branches
                                  → emits a structured synthesis_decision:
                                      • pick one rollout as-is, OR
                                      • cherry-pick / merge the best
                                        pieces across siblings into a
                                        single result branch
                                              ▼
                                    status:reviewing
                                  (normal AC review on the chosen artifact)
                                              ▼
                                       status:done
```

Key points worth knowing:

- Triggered by adding a literal `rollouts:N` label (N in 2..5) to the
  issue. No label, or `rollouts:1`, takes the legacy single-dev fast
  path that skips `synthesizing` entirely.
- Each rollout is a fully isolated dev run — its own worktree, branch,
  PR, and session — so failures in one rollout don't block siblings.
  `join.min_successes` (in the workflow YAML) decides when the join
  fires.
- The synthesize step is a **reduce**, not a vote. It runs the
  review-agent in `mode: synthesize`; the agent must return a
  structured decision (`chosen_rollout_index`, plus optional cherry-pick
  instructions). If the structured output is malformed, the Coordinator
  stops the reduce step rather than silently auto-picking — surfaces in
  the issue as a synthesize failure for human attention.
- Audit trail: `synthesis_decision` events plus per-rollout sessions
  visible via the audit/web UI (rollout_index/rollouts_total/rollout_group_id).
- The state graph is in `.github/workbuddy/workflows/default.md`; the
  rollout fan-out and `mode: synthesize` are first-class in the
  workflow YAML, not bolted on.

Use this when the AC admits multiple plausible implementations and you
want the agents to explore them in parallel, then keep the best one. For
straightforward issues, leave `rollouts:N` off and stick with the
single-dev path.

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
workbuddy cache invalidate --repo owner/name --issue N

# Or, if the poller cache + claim lease both need clearing (stuck after a crash)
workbuddy issue restart --repo owner/name --issue N \
  --coordinator http://127.0.0.1:8081 \
  --token-file /home/$USER/.config/workbuddy/auth-token
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
