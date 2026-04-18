---
name: pipeline-monitor
description: "Monitor the workbuddy agent pipeline, detect stuck issues, zombie processes, and missed redispatches. Works for both serve mode and distributed (coordinator+worker) mode. Use when the user says 'monitor the pipeline', 'check workbuddy status', '监工', '看看跑完了没', or asks to watch issue progress."
---

# Pipeline Monitor

Interactive skill that watches the workbuddy pipeline, diagnoses common
failure modes, and applies safe fixes. Works for both serve mode (single
process) and distributed mode (coordinator + workers).

## When to use

- User asks to monitor / watch / check the pipeline
- User suspects issues are stuck or agents are failing
- User wants a post-mortem on a completed batch
- Background monitoring notified you that an agent completed or failed

## Step 0: Detect deployment mode

Before monitoring, determine which mode workbuddy is running in:

```bash
# Check for coordinator process
ps aux | grep "workbuddy coordinator" | grep -v grep

# Check for serve process
ps aux | grep "workbuddy serve" | grep -v grep

# Check for worker processes
ps aux | grep "workbuddy worker" | grep -v grep
```

- **Serve mode**: single `workbuddy serve` process → use CLI tools + serve log
- **Distributed mode**: `workbuddy coordinator` + one or more `workbuddy worker` → use HTTP API + DB queries + process monitoring

## What to check (every pass)

### Common checks (both modes)

1. **GitHub issue labels** — the ground truth of pipeline state
   ```bash
   gh issue view N -R Owner/Repo --json labels,state,comments \
     --jq '{labels: [.labels[].name], state: .state, comments: (.comments | length)}'
   ```

2. **Agent processes**
   ```bash
   ps aux | grep -E "codex.*exec|claude.*exec" | grep -v grep
   ```

3. **Session logs** — what the agent is actually doing
   ```bash
   # Find session directories
   ls -lt .workbuddy/sessions/
   
   # Check latest activity in a session
   tail -3 .workbuddy/sessions/session-<ID>/codex-exec.jsonl | \
     python3 -c "import sys,json; [print(json.loads(l).get('item',{}).get('command','')[:100] or json.loads(l).get('item',{}).get('type','')) for l in sys.stdin]"
   ```

### Serve mode checks

4. **Task queue** (CLI)
   ```bash
   ./workbuddy status --tasks
   ```

5. **Recent events** (CLI)
   ```bash
   ./workbuddy status --events --since 10m
   ```

6. **Automated diagnosis** (CLI)
   ```bash
   ./workbuddy diagnose
   ./workbuddy diagnose --fix   # auto-apply safe fixes
   ```

### Distributed mode checks

4. **Coordinator health**
   ```bash
   curl -s http://coordinator:8081/health
   # Returns: {"repos":N,"status":"ok"}
   ```

5. **Registered repos**
   ```bash
   curl -s -H "Authorization: Bearer $TOKEN" http://coordinator:8081/api/v1/repos | python3 -m json.tool
   ```

6. **Task queue via DB** — most reliable way to see task state
   ```bash
   sqlite3 .workbuddy/workbuddy.db \
     "SELECT agent_name, status, worker_id, lease_expires_at, created_at
      FROM task_queue WHERE repo='Owner/Repo' ORDER BY created_at DESC LIMIT 10;"
   ```

7. **Events via DB** — the most complete audit trail
   ```bash
   sqlite3 .workbuddy/workbuddy.db \
     "SELECT type, issue_num, substr(payload,1,80), ts
      FROM events WHERE repo='Owner/Repo'
      ORDER BY ts DESC LIMIT 15;"
   ```
   Key event types: `state_entry`, `dispatch`, `completed`, `worker_registered`,
   `dispatch_blocked_by_dependency`, `poll_cycle_done`

8. **Coordinator log** — check for state transitions and dispatch events
   ```bash
   # If running in background, check the output file
   cat /path/to/coordinator-output.log
   # Key patterns to look for:
   #   [statemachine] state entry detected
   #   [statemachine] clearing prior inflight group
   #   [coordinator] task heartbeat DB update failed
   ```

8. **Worker log** — check for task pickup and agent launch
   ```bash
   cat /path/to/worker-output.log
   # Key patterns:
   #   [codex-debug] args=... env=... prompt=...  (agent started)
   #   [worker] received signal terminated         (clean shutdown)
   ```

## Common failure modes & fixes

### A. Issue not dispatched — missing `workbuddy` label
- **Symptom:** Issue has `status:developing` but no task in queue.
- **Fix:**
  ```bash
  gh issue edit N -R Owner/Repo --add-label workbuddy
  ```

### B. Task stuck in `running` with expired lease (distributed mode)
- **Symptom:** DB shows task `status=running`, `lease_expires_at` in the past, no codex process.
- **Cause:** Worker completed agent but failed to submit result (known bug #88).
- **Diagnosis:**
  ```bash
  sqlite3 .workbuddy/workbuddy.db \
    "SELECT id, agent_name, status, lease_expires_at FROM task_queue
     WHERE status='running' AND lease_expires_at < datetime('now');"
  ```
- **Fix:** Mark task as completed and restart worker:
  ```bash
  sqlite3 .workbuddy/workbuddy.db \
    "UPDATE task_queue SET status='completed', completed_at=CURRENT_TIMESTAMP
     WHERE id='<task-id>';"
  # Then restart the worker process
  ```

### C. Worker not picking up pending tasks
- **Symptom:** DB has `status=pending` task but worker is idle.
- **Cause:** Older expired `running` task with earlier `created_at` blocks the claim query.
- **Diagnosis:**
  ```bash
  sqlite3 .workbuddy/workbuddy.db \
    "SELECT id, agent_name, status, created_at FROM task_queue
     WHERE repo='Owner/Repo' AND status IN ('pending','running')
     ORDER BY created_at;"
  ```
- **Fix:** Complete or delete the stale running task (see B above), then
  restart the worker.

### D. Dev-agent self-blocks — missing Acceptance Criteria
- **Symptom:** Dev-agent completes quickly (~1 min), sets `status:blocked`.
- **Fix:** Edit issue body to add `## Acceptance Criteria` section, then:
  ```bash
  gh issue edit N -R Owner/Repo --remove-label 'status:blocked' --add-label 'status:developing'
  ```

### E. Agent completes but doesn't change labels
- **Symptom:** Task completed/codex exited, but issue labels unchanged.
- **Cause:** LLM didn't execute the label change command.
- **Fix (serve mode):** Auto-detected and redispatched.
- **Fix (distributed mode):** Manually change labels:
  ```bash
  gh issue edit N -R Owner/Repo --remove-label 'status:developing' --add-label 'status:reviewing'
  ```

### F. Review sends issue back to developing (normal retry)
- **Symptom:** Label flips `reviewing → developing`.
- **Fix:** Normal. Do not intervene. Max 3 retries, then stops.

### G. Coordinator not detecting label changes
- **Symptom:** Labels changed on GitHub but coordinator log shows no state entry.
- **Cause:** Poller hasn't run yet (poll_interval), or issue cache is stale.
- **Fix:** Wait for next poll cycle, or invalidate cache:
  ```bash
  ./workbuddy cache-invalidate --repo Owner/Repo --issue N
  ```

### H. Codex stuck in API inference (most common failure mode)
- **Symptom:** Codex process alive, 0.5% CPU, no child processes, no JSONL output for 10+ min.
- **Cause:** Codex enters a long LLM inference that never completes. Happens after agent finishes work (labels already changed) or mid-work.
- **Diagnosis:**
  ```bash
  # Check JSONL staleness
  for f in $(find .workbuddy -name "codex-exec.jsonl" -newer /tmp/workbuddy-worker*.log); do
    age=$(( $(date +%s) - $(stat -c '%Y' "$f") ))
    if [ $age -gt 600 ]; then echo "STALE ($age s): $f"; fi
  done
  # Confirm no child processes
  pstree -p <codex-pid>  # only threads = stuck
  ```
- **Fix:** Kill codex, check if labels changed. If yes → mark task completed. If no → mark task failed + invalidate cache. Then restart worker.

### I. Worker hangs after codex is killed
- **Symptom:** After killing stuck codex, worker doesn't claim next task. Heartbeat errors in coordinator log: "task already completed".
- **Cause:** Worker's heartbeat goroutine doesn't exit when codex dies unexpectedly.
- **Fix:** Kill the worker process (`kill -9`), clean up stale running tasks in DB, start new worker.

### J. Issue claim stuck after coordinator crash
- **Symptom:** No dispatch after cache-invalidate; `diagnose` shows a claim still held by a dead coordinator/worker.
- **Cause:** Per-issue claim (REQ-057/059) wasn't released cleanly.
- **Self-heal:** Claims have a TTL — wait for expiry and the next poll overwrites with a `claim_expired` event.
- **Force:** `workbuddy recover` clears stale runtime state (processes, worktrees, claims).

### K. Consecutive-failure cap reached (REQ-055)
- **Symptom:** `workbuddy diagnose` reports "dev-agent has failed 3 times in a row"; dispatch stops.
- **First check:** is it infra or verdict? Read the latest few comments on the issue:
  - "Infra Error" header → launcher/runtime crash (REQ-056). Fix infra, `cache-invalidate`, retry.
  - "Failure" header → agent disagrees with the AC. Tighten AC or intervene manually.
- **Reset:** fix the root cause, flip label back to `status:developing`, `cache-invalidate`.

### L. Worktree setup failed — worker refuses to run
- **Symptom:** Issue comment: "worktree setup failed"; task marked failed.
- **Cause:** Stale `.workbuddy/worktrees/issue-N/` metadata, dirty tree, or wrong branch (REQ-058 refuses CWD fallback by design).
- **Fix:** `workbuddy recover` (prunes stale worktrees) then re-dispatch.

## Monitoring strategies

### Strategy 1: Watch specific issue via GitHub polling
```bash
# Poll issue state every 60s
watch -n 60 "gh issue view N -R Owner/Repo --json labels --jq '[.labels[].name]'"
```

### Strategy 2: Monitor session log growth (agent is working)
```bash
# Track codex activity
watch -n 10 "wc -l .workbuddy/sessions/session-*/codex-exec.jsonl 2>/dev/null"
```

### Strategy 3: Monitor with until-loop (wait for completion)
```bash
until echo "$(gh issue view N -R Owner/Repo --json labels --jq '[.labels[].name]')" | grep -q "status:done"; do
  sleep 30
done
echo "ISSUE_COMPLETED"
```

### Strategy 4: DB-based task monitoring (distributed mode)
```bash
watch -n 15 "sqlite3 .workbuddy/workbuddy.db \
  'SELECT agent_name, status, worker_id FROM task_queue
   WHERE repo=\"Owner/Repo\" ORDER BY created_at DESC LIMIT 5;'"
```

## Decision tree

```
Detect mode (serve vs distributed)
│
├─ Check GitHub labels for target issues
│  ├─ All status:done → Report completion. Stop.
│  ├─ status:developing/reviewing with active agent process → Normal. Monitor.
│  └─ Label unchanged for >10 min with no agent process → Investigate below
│
├─ Check task queue (CLI or DB)
│  ├─ Task running + codex process alive → Normal. Wait.
│  ├─ Task running + no codex process → Result submission failed (fix B)
│  ├─ Task pending + worker alive → Worker may be re-claiming stale task (fix C)
│  └─ No pending tasks + labels unchanged → Cache stale or missing workbuddy label
│
└─ Check logs (coordinator/worker/serve)
   ├─ "state entry detected" → Dispatch happened, check worker
   ├─ "heartbeat DB update failed" → Task already completed/released
   └─ No recent log entries → Poller or worker may be stuck
```

## Reporting template

```markdown
## Pipeline Status Report

| Issue | Repo | Status | Agent | Retry | Notes |
|-------|------|--------|-------|-------|-------|
| #37 | AegisLab | reviewing | review-agent | 2/3 | In progress |
| #88 | workbuddy | developing | dev-agent | 0/3 | Started |

**Mode:** distributed (coordinator:8081 + 2 workers)
**Repos:** Lincyaw/workbuddy, OperationsPAI/AegisLab
**Agent processes:** 1 (codex)
**Interventions:** Fixed stale task for #37 dev-agent (bug #88)
```

## Arguments

Optional: `<owner/repo> <issue-numbers...>`
Example: `/pipeline-monitor OperationsPAI/AegisLab 37 42`
If omitted, monitor all repos and all open workbuddy-labeled issues.
