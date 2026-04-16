---
name: pipeline-monitor
description: "Monitor the workbuddy agent pipeline, detect stuck issues, zombie processes, and missed redispatches. Use when the user says 'monitor the pipeline', 'check workbuddy status', '监工', '看看跑完了没', or asks to watch issue progress."
user_invocable: true
---

# Pipeline Monitor

Interactive skill that watches the workbuddy coordinator/worker pipeline,
diagnoses common failure modes, and applies safe fixes.

## When to use

- User asks to monitor / watch / check the pipeline
- User suspects issues are stuck or agents are failing
- User wants a post-mortem on a completed batch
- Background monitoring notified you that an agent completed or failed

## What to check (every pass)

Run these in parallel whenever possible. **Prefer native CLI tools over raw
sqlite3 queries.**

1. **Task queue**
   ```bash
   .workbuddy/workbuddy status --tasks --repo Lincyaw/workbuddy
   ```
   Add `--status running` to filter active tasks, `--status failed` for failures.

2. **Recent events**
   ```bash
   .workbuddy/workbuddy status --events --repo Lincyaw/workbuddy --since 10m
   ```
   Filter by type: `--type state_entry`, `--type dispatch_blocked_by_dependency`, etc.

3. **Automated diagnosis**
   ```bash
   .workbuddy/workbuddy diagnose --repo Lincyaw/workbuddy
   ```
   Add `--fix` to auto-apply safe fixes (cache invalidation for stuck issues).

4. **Agent process count**
   ```bash
   ps aux | grep -c '[c]odex'
   ```

5. **GitHub issue labels** (for specific issues)
   ```bash
   gh issue view N --repo Lincyaw/workbuddy --json labels,state,title
   ```

6. **New PRs**
   ```bash
   gh pr list --repo Lincyaw/workbuddy --limit 20 --json number,title,state,createdAt
   ```

7. **Serve log** (only when CLI tools don't explain the situation)
   ```bash
   tail -n 30 .workbuddy/serve.log
   ```

## Common failure modes & fixes

### A. Issue not dispatched — missing `workbuddy` trigger label
- **Symptom:** Issue has `status:developing` but no task in queue, no dispatch events.
- **Cause:** Workflow trigger requires `issue_label: "workbuddy"`.
- **Fix:**
  ```bash
  gh issue edit N --repo Lincyaw/workbuddy --add-label workbuddy
  .workbuddy/workbuddy cache-invalidate --repo Lincyaw/workbuddy --issue N
  ```

### B. Issue not dispatched — stale issue_cache
- **Symptom:** Issue has correct labels, verdict is `ready`, but no task in queue.
- **Fix:**
  ```bash
  .workbuddy/workbuddy cache-invalidate --repo Lincyaw/workbuddy --issue N
  ```

### C. Dependency blocked — upstream not done
- **Symptom:** `status --events` shows `dependency_verdict_changed` with `verdict: blocked`.
- **Fix:** Normal. Wait for upstream. After upstream reaches `done`, the dependency
  resolver will auto-invalidate cache and trigger redispatch (no manual intervention needed).

### D. Dev-agent self-blocks — missing `## Acceptance Criteria` header
- **Symptom:** Dev-agent completes quickly (~1 min), sets `status:blocked`.
- **Cause:** Dev-agent prompt requires literal `## Acceptance Criteria` header.
- **Fix:** Edit issue body to use exact header, then:
  ```bash
  gh issue edit N --repo Lincyaw/workbuddy --remove-label 'status:blocked' --add-label 'status:developing'
  .workbuddy/workbuddy cache-invalidate --repo Lincyaw/workbuddy --issue N
  ```

### E. Transient `codex: no such file or directory`
- **Symptom:** serve log shows `fork/exec .../bin/codex: no such file or directory`
- **Fix:** serve has auto-retry (60s delay). If task fails after retries,
  use `cache-invalidate` to force redispatch.

### F. Agent completes but doesn't change labels
- **Symptom:** Task completed, but issue label unchanged (state machine stalled).
- **Cause:** LLM non-determinism — agent output judgment but didn't execute `gh issue edit`.
- **Fix:** Automatic — serve now detects unchanged labels and auto-invalidates cache
  for redispatch. Look for `labels unchanged — redispatching` in serve log.

### G. Review-agent sends issue back to developing
- **Symptom:** Issue label flips from `reviewing` → `developing`
- **Fix:** Normal back-edge. Do not intervene.

### H. Zombie processes (rare after WaitDelay fix)
- **Symptom:** Processes exist but CPU frozen and task completed/failed.
- **Fix:** Wait 5+ minutes. If CPU still frozen, `kill -9 <pid>`.

### I. Worktree remove failure
- **Symptom:** log shows `致命错误：... 不是一个工作区: exit status 128`
- **Fix:** Workspace manager falls back to `Prune()`. No action needed.

## Monitoring strategies

### Strategy 1: `status --watch` (preferred for specific issues)
```bash
.workbuddy/workbuddy status --watch --repo Lincyaw/workbuddy --issue 67 --timeout 30m
```

### Strategy 2: `Monitor` with serve log (for broad watching)
```bash
tail -n 0 -f .workbuddy/serve.log | grep -E --line-buffered '(state entry detected|agent.*(completed|failed)|labels unchanged|dependency unblocked|no such file|redispatch)'
```

### Strategy 3: `ScheduleWakeup` (periodic check-ins)
Stay under 300s to keep cache warm.

### Anti-pattern: manual sleep polling
Use `Monitor` with `until` loop instead.

## Decision tree

```
Run: status --tasks + status --events --since 5m + diagnose
│
├─ All issues status:done, task queue empty → Report completion. Stop monitors.
├─ Task running, matching label → Normal. Use --watch or Monitor.
├─ Log stale > 5 min, no agent processes → Run diagnose --fix.
├─ Issue developing, verdict=ready, no task → cache-invalidate.
├─ Issue developing but missing workbuddy label → Add label + cache-invalidate.
├─ Verdict blocked, upstream done → cache-invalidate blocked issue.
└─ Repeated failures (>3 times) → Escalate to user.
```

## Reporting template

```markdown
| Issue | Status | Agent | Notes |
|-------|--------|-------|-------|
| #67 | done | — | Review passed |
| #68 | done | — | Review passed |
| #69 | developing | dev-agent | Running |
| #70 | blocked | — | Depends on #69 |
```

- **Agent processes:** N
- **New PRs:** Yes/No (list numbers)
- **Interventions:** None / cache-invalidate for #X

## Arguments

Optional space-separated issue numbers, e.g. `/pipeline-monitor 67 68 69 70`.
If omitted, monitor all `workbuddy`-labeled issues not `status:done`.
