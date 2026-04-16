---
name: pipeline-monitor
description: "Monitor the workbuddy agent pipeline, detect stuck issues, zombie codex processes, and missed redispatches. Use when the user says 'monitor the pipeline', 'check workbuddy status', '监工', '看看跑完了没', or asks to watch codex processes / issue labels / serve logs."
user_invocable: true
---

# Pipeline Monitor

Interactive skill that watches the workbuddy coordinator/worker pipeline,
diagnoses common failure modes, and applies safe fixes.

## When to use

- User asks to monitor / watch / check the pipeline
- User suspects issues are stuck or agents are failing
- User wants a post-mortem on a completed batch
- Background monitoring woke you up because codex processes ended

## What to check (every pass)

Run these in parallel whenever possible:

1. **Codex process count**
   ```bash
   ps aux | grep -c '[c]odex'
   ```
   - 0 → pipeline may be idle (verify with task queue)
   - >0 → note which issues are active

2. **Serve log tail** (last 20–30 lines)
   ```bash
   tail -n 30 .workbuddy/serve.log
   ```
   Look for:
   - `agent dev-agent completed` / `agent review-agent completed`
   - `agent ... failed: launcher: codex-exec: start: fork/exec ...: no such file or directory`
   - `state entry detected: ... entered "reviewing"`
   - `state entry detected: ... entered "developing"`
   - `workspace removed worktree ...`
   - Any `stuck_detected` or `cycle_limit_reached` events

3. **GitHub issue labels for the batch**
   ```bash
   gh issue view N --repo Lincyaw/workbuddy --json labels,state,title
   ```
   Expected flow:
   - `status:developing` → dev-agent running
   - `status:reviewing` → review-agent running
   - `status:done` → complete
   - `status:developing` after reviewing → review-agent requested changes (normal back-edge)

4. **New PRs**
   ```bash
   gh pr list --repo Lincyaw/workbuddy --limit 20 --json number,title,state,createdAt
   ```

5. **Task queue (SQLite)**
   ```bash
   sqlite3 .workbuddy/workbuddy.db "SELECT issue_num, agent_name, status, updated_at FROM task_queue WHERE status IN ('running','pending') ORDER BY issue_num;"
   ```
   Also check recent failures:
   ```bash
   sqlite3 .workbuddy/workbuddy.db "SELECT issue_num, agent_name, status, updated_at FROM task_queue WHERE status='failed' AND issue_num BETWEEN X AND Y;"
   ```

6. **Stuck / blocked events (optional)**
   ```bash
   sqlite3 .workbuddy/workbuddy.db "SELECT type, repo, issue_num, ts FROM events WHERE type IN ('stuck_detected','cycle_limit_reached','dispatch_blocked_by_dependency') AND ts > 'YYYY-MM-DD HH:MM:SS' ORDER BY ts DESC;"
   ```

## Common failure modes & fixes

### A. Transient `codex: no such file or directory`
- **Symptom:** serve log shows `fork/exec .../bin/codex: no such file or directory`
- **Fix:** `cmd/serve.go` already has an auto-retry goroutine (60s delay). **Do not manually redispatch.** Just wait and verify the retry succeeds in the next log tail.

### B. Dependency resolver blocked redispatch
- **Symptom:** Issues #47–#51 (or similar) were stuck in `status:developing` after their upstream dependencies (#39, #40, etc.) became `done`. The state machine does not auto-redispatch when a dependency verdict changes from `blocked` → `ready` because the poller only emits events on label changes.
- **Fix:** Force cache invalidation so the poller treats them as new:
  ```bash
  sqlite3 .workbuddy/workbuddy.db "DELETE FROM issue_cache WHERE repo='Lincyaw/workbuddy' AND issue_num IN (47,48,49,50,51);"
  ```
  On the next poll cycle the poller will emit `EventIssueCreated` and dispatch dev-agents.

### C. Review-agent sends issue back to developing
- **Symptom:** Issue label flips from `reviewing` → `developing`
- **Fix:** This is the **Agent-as-Router** back-edge. The review-agent found failing acceptance criteria and is requesting revisions. **Do not intervene.** The state machine will dispatch dev-agent automatically.

### D. Worktree remove failure
- **Symptom:** log shows `致命错误：... 不是一个工作区: exit status 128`
- **Fix:** The workspace manager already falls back to `Prune()`. No action needed unless the directory piles up.

### E. Zombie codex processes
- **Symptom:** Processes exist but CPU is 0.0% and log shows the agent already completed/failed.
- **Fix:** Check PPID. If orphaned, `kill -9 <pid>`. In practice, codex processes in this repo are well-parented by the serve process and usually self-terminate.

## Loop strategy (how to self-monitor)

Use one of these two patterns — **never** `sleep` in a busy loop.

### Pattern 1: `ScheduleWakeup` (preferred for long waits)
When agents are running and you expect them to take minutes:
```
ScheduleWakeup with delaySeconds=120–180
prompt: "Continue monitoring the workbuddy pipeline..."
```
This keeps the cache warm and avoids burning context on idle polling.

### Pattern 2: `Monitor` with log tail + grep (for event-driven watching)
Use when you want to be notified the instant something interesting happens:
```bash
tail -n 0 -f .workbuddy/serve.log | grep -E --line-buffered '(issue=Lincyaw/workbuddy#(47|49|51)|state entry detected: Lincyaw/workbuddy#(47|49|51)|agent (dev-agent|review-agent) (failed|completed))'
```
Set `persistent=false` and a reasonable `timeout_ms` (e.g. 600000).

### Anti-pattern: manual sleep polling
Do **not** do:
```bash
sleep 30 && tail ...
sleep 30 && tail ...
```
The runtime blocks this. If you must poll, use `Monitor` with an `until` loop.

## Decision tree (when to intervene)

```
Check labels + task queue + log
│
├─ All issues are status:done and task queue empty
│  → Pipeline idle. Report completion. Stop monitors.
│
├─ Issue has label reviewing/developing and task is running
│  → Check log for recent activity.
│     ├─ Log active (worktree create/remove, codex-debug)
│     │  → Normal. Wait (ScheduleWakeup or Monitor).
│     └─ Log stale > 3 min AND no codex processes
│        → Possibly stuck. Check if agent crashed without logging.
│          Consider deleting issue_cache to force re-evaluation.
│
├─ Issue label = done but task still running
│  → Race condition. Usually resolves next poll. Wait one cycle.
│
├─ Issue label = developing, upstream deps are done, no task running
│  → Dependency unblock missed redispatch. Delete issue_cache row.
│
└─ Repeated failures on same issue (>3 times same agent)
   → May be a real bug or persistent env issue. Escalate to user.
```

## Reporting template

When you report back to the user, use a concise table:

```markdown
| Issue | Status | Agent | Notes |
|-------|--------|-------|-------|
| #47 | done | — | Review passed |
| #48 | done | — | Review passed |
| #49 | developing | dev-agent | Back from reviewing (revisions requested) |
| #50 | reviewing | review-agent | Retry running |
| #51 | developing | dev-agent | Still running |
```

Then state:
- **Codex processes:** N
- **New PRs:** Yes/No (list numbers)
- **Interventions:** None / Deleted issue_cache for #X / Killed zombie pid Y

## Arguments

This skill accepts an optional space-separated list of issue numbers to focus on,
e.g. `/pipeline-monitor 47 48 49 50 51`. If omitted, monitor all `workbuddy`-labeled
issues that are not `status:done`.
