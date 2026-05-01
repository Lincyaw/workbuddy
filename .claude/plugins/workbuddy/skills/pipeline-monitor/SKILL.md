---
name: pipeline-monitor
description: "Monitor the workbuddy agent pipeline, detect stuck issues, zombie processes, and missed redispatches. Works for both the 3-unit bundle (supervisor + coordinator + worker) and single-process `serve`. Use when the user says 'monitor the pipeline', 'check workbuddy status', '监工', '看看跑完了没', or asks to watch issue progress."
user_invocable: true
---

# Pipeline Monitor

Interactive skill that watches the workbuddy pipeline, diagnoses common
failure modes, and applies safe fixes. Works for both the v0.5+ default
**bundle** topology (`supervisor` + `coordinator` + `worker` units) and
the legacy single-process **serve** mode.

## When to use

- User asks to monitor / watch / check the pipeline
- User suspects issues are stuck or agents are failing
- User wants a post-mortem on a completed batch
- Background monitoring notified you that an agent completed or failed

## Step 0: Detect deployment topology

```bash
ps -eo pid,cmd | grep -E "workbuddy (supervisor|coordinator|worker|serve)" | grep -v grep
```

- **Bundle** (default since v0.5): three processes — `workbuddy supervisor`
  + `workbuddy coordinator` + `workbuddy worker`. The supervisor owns the
  agent subprocesses behind a unix socket, so the worker can be killed and
  restarted without losing in-flight Claude/Codex runs.
- **Serve**: a single `workbuddy serve` process. Killing it kills agents.

For systemd installs, also run:

```bash
systemctl --user status workbuddy-supervisor workbuddy-coordinator workbuddy-worker 2>/dev/null \
  || systemctl --user status workbuddy
```

The CLI surface is identical across both topologies — only the failure
recovery details differ (see "Bundle vs serve recovery" below).

## What to check (every pass)

1. **Issue state via CLI** — what workbuddy thinks is happening
   ```bash
   workbuddy status --repo Owner/Repo
   workbuddy status --stuck                 # issues idle >1h, or cycle counts > 0
   workbuddy status --tasks --status running
   workbuddy status --events --since 10m    # audit trail
   ```

2. **Automated diagnosis** — surfaces common patterns with suggested fixes
   ```bash
   workbuddy diagnose                      # scan
   workbuddy diagnose --fix                # apply only safe fixes (cache invalidation etc.)
   workbuddy diagnose --repo Owner/Repo --format json   # for piping
   ```

3. **GitHub labels** — the ground truth, in case the local DB is stale
   ```bash
   gh issue view N -R Owner/Repo --json labels,state,comments \
     --jq '{labels: [.labels[].name], state: .state, comments: (.comments | length)}'
   ```

4. **Agent processes** — what's actually running on the host
   ```bash
   ps -eo pid,etime,pcpu,cmd | grep -E "codex.*exec|claude.*exec" | grep -v grep
   ```

5. **Hooks** — operator-owned event hooks (cap-hit alerts, Slack pings, etc.)
   ```bash
   workbuddy hooks list
   workbuddy hooks status --coordinator http://127.0.0.1:8081 \
     --token-file /home/$USER/.config/workbuddy/auth-token
   ```

6. **Operator incident inbox** — coordinator drops files here when something
   needs human/Claude attention (cap-hit, lost worker, dispatch refusal)
   ```bash
   ls -lt ~/.workbuddy/operator/inbox/ 2>/dev/null
   # operator-watch tails this and spawns Claude per incident
   ```

## Block until something happens

Prefer `status --watch` over hand-rolled `until` loops:

```bash
workbuddy status --watch --repo Owner/Repo --issue N --timeout 30m
```

For a free-form wait on labels:

```bash
until gh issue view N -R Owner/Repo --json labels --jq '[.labels[].name]' \
      | grep -q "status:done"; do sleep 30; done
echo "ISSUE_COMPLETED"
```

## Direct DB peeks (when CLI isn't enough)

Useful columns added since v0.4: `claim_token`, `acked_at`,
`heartbeat_at`, `rollout_index`, `rollouts_total`, `rollout_group_id`.

```bash
sqlite3 .workbuddy/workbuddy.db <<'SQL'
SELECT agent_name, status, worker_id, heartbeat_at, lease_expires_at, created_at
  FROM task_queue
 WHERE repo='Owner/Repo'
 ORDER BY created_at DESC LIMIT 10;
SQL
```

Useful event types: `state_entry`, `dispatch`, `completed`,
`worker_registered`, `dispatch_blocked_by_dependency`, `claim_expired`,
`poll_cycle_done`, `token_usage`.

## Common failure modes & fixes

### A. Issue not dispatched — missing `workbuddy` label
Symptom: issue has `status:developing` but no task in queue.
```bash
gh issue edit N -R Owner/Repo --add-label workbuddy
```

### B. Task stuck `running` with expired lease and no agent process
Symptom: `task_queue.status=running`, `lease_expires_at` in the past,
no codex/claude process for it.

In **bundle** mode this is rare — the supervisor reconciles surviving
agents back to the worker on reattach. The right fix is to let the
coordinator's stale-claim sweep run, or force it:

```bash
workbuddy issue restart --repo Owner/Repo --issue N \
  --coordinator http://127.0.0.1:8081 \
  --token-file /home/$USER/.config/workbuddy/auth-token
workbuddy cache invalidate --repo Owner/Repo --issue N
```

In **serve** mode (no supervisor), if the worker died mid-run there is no
one to reconcile. Prefer `workbuddy recover --kill-zombies --prune-worktrees`
followed by `issue restart`, rather than hand-editing `task_queue` rows
(claim tokens / heartbeats interact in non-obvious ways).

### C. Worker not picking up a `pending` task
Symptom: a `pending` task exists, but the worker is idle. Often an older
`running` row with a stale lease blocks the claim query.
```bash
workbuddy diagnose --fix          # reaps stale claims, invalidates cache
# If still stuck after the next poll cycle:
workbuddy issue restart --repo Owner/Repo --issue N
```

### D. Dev-agent self-blocks — missing Acceptance Criteria
Symptom: dev-agent completes in ~1 min and sets `status:blocked`. Edit
the issue body to add a `## Acceptance Criteria` section, then:
```bash
gh issue edit N -R Owner/Repo --remove-label 'status:blocked' --add-label 'status:developing'
workbuddy cache invalidate --repo Owner/Repo --issue N
```

### E. Agent completes but doesn't change labels
Symptom: task `completed`, agent process gone, but labels unchanged. The
LLM didn't run the routing `gh issue edit`. The coordinator self-heals
this on the next poll, but you can shortcut:
```bash
gh issue edit N -R Owner/Repo --remove-label 'status:developing' --add-label 'status:reviewing'
```

### F. Review sends issue back to developing (normal retry)
Label flips `reviewing → developing`. Do not intervene. The 3-cycle cap
(REQ-055) auto-stops dispatch and posts a needs-human comment.

### G. Coordinator not detecting label changes
Symptom: labels changed on GitHub but no `state_entry` event.
```bash
workbuddy cache invalidate --repo Owner/Repo --issue N
```

### H. Codex stuck in API inference (most common runtime failure)
Symptom: codex process alive at ~0% CPU, no children, no JSONL writes
for 10+ min. Often after the agent has already changed labels.
```bash
# Find the live session log
ls -lt .workbuddy/sessions/ | head
# Confirm staleness
for f in .workbuddy/sessions/*/codex-exec.jsonl; do
  age=$(( $(date +%s) - $(stat -c '%Y' "$f") ))
  [ $age -gt 600 ] && echo "STALE ($age s): $f"
done
# Confirm no child processes
pstree -p <codex-pid>
```
Fix: kill the codex pid. In **bundle** mode the worker reattaches and
records a clean exit; in **serve** mode you may need
`workbuddy recover --kill-zombies` to clean up. Then check labels — if
they already advanced, just `cache invalidate` and let the next poll
move on; otherwise `workbuddy issue restart`.

### I. Issue claim stuck after coordinator crash (REQ-057/059)
Symptom: no dispatch even after `cache invalidate`; `diagnose` shows a
claim still held by a dead coordinator/worker.
- Self-heal: claims have a TTL — wait for expiry, the next poll emits
  `claim_expired` and overwrites.
- Force: `workbuddy recover` clears local runtime state; for a remote
  coordinator's in-memory inflight tracker, use
  `workbuddy issue restart --coordinator <url>`.

### J. Consecutive-failure cap reached (REQ-055)
Symptom: `diagnose` reports "dev-agent has failed 3 times in a row",
dispatch stops, issue gets a needs-human comment.
- Read the latest comments: **"Infra Error"** header → launcher/runtime
  crash (REQ-056), doesn't burn the budget; fix infra, then restart. A
  plain **"Failure"** header → agent disagrees with the AC; tighten AC
  or intervene.
- Reset: fix the root cause, flip label back to `status:developing`,
  `workbuddy cache invalidate --repo Owner/Repo --issue N`.

### K. Worktree setup failed — worker refuses to fall back to CWD (REQ-058)
Symptom: issue comment "worktree setup failed"; task marked failed.
```bash
workbuddy recover --prune-worktrees --dry-run    # preview
workbuddy recover --prune-worktrees              # apply
workbuddy issue restart --repo Owner/Repo --issue N
```

### M. Issue not dispatching because a dependency is blocking it
Symptom: issue is `status:developing` with the `workbuddy` label, no
agent process, no `dispatch` event in `workbuddy status --events`. The
issue may also carry a 😕 reaction added by workbuddy.

The Coordinator gates dispatch on `workbuddy.depends_on` declared in the
issue body (YAML block under `## Dependencies`). When verdict is
`blocked` or `needs_human`, dispatch is refused and a
`dispatch_blocked_by_dependency` event is logged.

Diagnose:
```bash
workbuddy status --repo Owner/Repo            # CYCLES + DEPENDENCY columns
workbuddy status --events --type dispatch_blocked_by_dependency --since 1h
workbuddy diagnose --repo Owner/Repo          # surfaces malformed/cyclic deps
gh issue view N -R Owner/Repo --json body --jq .body | grep -A5 'workbuddy:'
```

Fix by intent:
- **Blocker really not done yet** → wait, or work the upstream issue.
- **Blocker actually done, declaration stale** → edit the issue body to
  drop the satisfied `depends_on` ref, then
  `workbuddy cache invalidate --repo Owner/Repo --issue N`.
- **Need to override a real dependency for one run** → add the
  `override:force-unblock` label (verdict flips to `override`). Don't
  hand-toggle `status:blocked` — the Coordinator owns that label.
- **Malformed `depends_on` ref** → `diagnose` points at the offending
  line; fix the YAML, `cache invalidate`.
- **Cross-repo ref** (`owner/other#N`) — accepted-but-unsupported in
  v0.1; verdict goes to `needs_human` until the upstream lands or the
  ref is removed.

### L. Worker restart didn't kill agents — that's by design (bundle only)
Bundle mode preserves agent runs across worker restarts via the
supervisor socket. If you `systemctl --user restart workbuddy-worker`
and `ps` still shows codex/claude — that's correct, not a leak. The new
worker reattaches and resumes the JSONL stream.

## Bundle vs serve recovery cheat sheet

| Symptom | Bundle (default) | Serve |
|---|---|---|
| Worker crashed mid-task | Supervisor keeps the agent; worker reattaches on restart | Agent dies with the process; use `recover --kill-zombies` |
| Need to upgrade workbuddy without dropping runs | `workbuddy deploy upgrade --name workbuddy-worker` | Drain or accept downtime |
| Local runtime cleanup | `workbuddy recover --prune-worktrees` (don't `--kill-zombies` lightly — supervisor is managing them) | `workbuddy recover --kill-zombies --prune-worktrees` |

## Decision tree

```
Detect topology (bundle vs serve)
│
├─ workbuddy diagnose --repo … (start here, every time)
│
├─ Check GitHub labels for target issues
│  ├─ All status:done → Report. Stop.
│  ├─ status:developing/reviewing + active agent process → Normal. Monitor.
│  └─ Label unchanged for >10 min with no agent process → see fixes B/E/H
│
├─ workbuddy status --tasks --status running / --stuck
│  ├─ running + agent alive → Normal. Wait or `status --watch`.
│  ├─ running + no agent → fix B
│  ├─ pending + worker alive → fix C
│  └─ no pending + labels unchanged → fix A or G
│
└─ Logs / journal
   ├─ journalctl --user -u workbuddy-coordinator -n 100   # bundle
   ├─ journalctl --user -u workbuddy-worker      -n 100   # bundle
   └─ journalctl --user -u workbuddy             -n 100   # legacy serve
```

## Reporting template

```markdown
## Pipeline Status Report

| Issue | Repo | State | Cycles | Agent | Notes |
|-------|------|-------|--------|-------|-------|
| #37 | AegisLab | reviewing | 2/3 | review-agent | In progress |
| #88 | workbuddy | developing | 0/3 | dev-agent | Started |

**Topology:** bundle (supervisor + coordinator@127.0.0.1:8081 + 1 worker)
**Repos:** Lincyaw/workbuddy, OperationsPAI/AegisLab
**Live agents:** 1 codex
**Interventions:** `issue restart` for #37 after stale claim
```

## Arguments

Optional: `<owner/repo> <issue-numbers...>`
Example: `/pipeline-monitor OperationsPAI/AegisLab 37 42`
If omitted, monitor all repos and all open workbuddy-labeled issues.
