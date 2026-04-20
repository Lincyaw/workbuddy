# Codex Lifecycle — Symptom Catalog (for design discussion)

Captured 2026-04-19 after the #141/#142/#143 fixes landed but `running=11 codex=10 pending=10`
is still being reported by the slot-vs-codex monitor. The individual patches closed specific
holes; the overall lifecycle model is still loose. This document is the starting material for
the design conversation — it is *not* a plan.

## 1. Runtime invariants we want but don't have

At steady state we'd like:

- `goroutines_holding_sem == db_tasks_running == live_codex_children` (all three equal).
- A task's semaphore slot is released within one heartbeat cycle of the child exiting, no matter why it exited.
- A task's DB row transitions out of `running` exactly when the goroutine releases the slot, not earlier, not later.
- Cross-restart of either the worker or the coordinator must not produce two live goroutines for the same `task_id`.

In practice each of those invariants breaks in at least one scenario below.

## 2. Empirical symptoms (observed, not theorized)

| # | Symptom | Observed signature |
|---|---------|--------------------|
| a | `wsMgr.Create` slow on fresh worktree → lease (30s) expires before heartbeat starts → coordinator re-dispatches same task to the same worker | Two goroutines with same `task_id`, two `codex-exec` children |
| b | Scanner goroutine blocks on `events <- evt` when buffer is full → `wg.Wait` hangs forever → `session.Run` never returns even after child exit | `ps` shows no codex, but worker log silent; goroutine leaked |
| c | Proxy goroutine blocks on `eventsCh <- evt` → `<-proxyDone` never fires after `close(sessionCh)` | Worker stuck in post-session cleanup |
| d | `waitEvents()` (per-session log drain) takes unbounded time | Cleanup hung before `Verify`/`SubmitResult` ever runs |
| e | `rep.Verify` (shells out to `gh`) + `client.SubmitResult` each take seconds | Steady `running − codex ≈ 3` gap during healthy operation |
| f | Zombie DB-running row whose codex is dead, yet heartbeats continue | Monitor shows `running > codex` for hours; `diagnose` didn't flag it |
| g | Manual DB cleanup (`UPDATE status='completed'`) does NOT cancel the in-flight goroutine | After cleanup, worker reports `running=10` again within seconds — same goroutines, new phantom rows or stuck semaphore |
| h | Orphan codex child: DB says terminal, codex process still alive under worker PID | Leaks until process hits `WaitDelay` / natural exit |
| i | Coordinator restart → `issue_claim_expired` event → task re-dispatched to a *different* worker while first is still running | Double-execution seen in #32/#36 review phase |
| j | `ClaimNextTask` SQL does not exclude `worker_id = self`; an expired-lease task is claimable by the original worker | Self-reclaim races with a still-running goroutine |
| k | Current live state: `running=11 codex=10 pending=10 match=N` persistent | One of (f)–(j) is still firing; fixes (b)–(d) bounded the drain but did not eliminate the skew |

## 3. Partial fixes already landed (branch `fix/dup-claim-race-141-142-143`)

- Heartbeat + taskCtx are now set up BEFORE `wsMgr.Create` → closes (a) for common case.
- `isTaskOwnershipLost` detection in heartbeat → cancels `taskCtx` when coordinator says the task was taken, unblocking the goroutine.
- `codexScannerDrainTimeout = 5s` after `cmd.Wait` with fallback `cancel()` → bounds (b).
- `postSessionDrainTimeout = 5s` around `<-proxyDone` with `taskCancel()` fallback → bounds (c).
- `waitEvents` bounded 5s → bounds (d).
- `TransitionTaskStatusIfRunning` CAS + 409 in coordinator submit → rejects late zombie submits that overwrite terminal rows.
- `diagnose` issue-state cross-check → flags tasks whose `task.State` ≠ current issue label.

None of these establish invariants (§1); they only truncate the worst tails.

## 4. Unresolved — the topics to design around

### 4.1 Ownership model
- **Question:** What is the single source of truth for "this task is mine"?
  - Today it's split: DB row (`worker_id`, `lease_expires_at`), in-memory goroutine, and an OS child process. They can disagree.
- Options to discuss: supervised process-per-task with PID pinned in DB; a per-task `owner_token` checked before every side-effect; a heartbeat that also verifies child PID is alive.

### 4.2 Cancellation propagation
- **Question:** When the coordinator revokes ownership (heartbeat says 409, or admin cleanup), what must happen locally?
  - Today: we `taskCancel()`, but the goroutine may still be inside a blocking call that doesn't honor the context (e.g., `gh` CLI, `session.Run` buffered channel, external worktree IO).
- Options: kill the codex child process directly, not just its context; SIGKILL fallback after N seconds; make `Verify`/`SubmitResult` ctx-aware and interruptible.

### 4.3 Slot accounting
- **Question:** Should the semaphore slot be tied to the goroutine or to the DB row?
  - Today it's the goroutine — so DB cleanup doesn't free it (symptom g).
- Options: reconcile slot count from DB every heartbeat; maintain a worker-local registry indexed by task_id; expose `/api/v1/worker/debug/slots` with PID per slot for external audit.

### 4.4 Admin operations contract
- **Question:** What is the supported way to "kill a runaway task"?
  - Today we tell ops to `UPDATE task_queue` by hand, which is the exact thing that produced symptom (g).
- Options: a `POST /api/v1/tasks/{id}/abort` endpoint that marks + notifies the worker to `taskCancel` + SIGKILL; document that DB edits alone are *not* sufficient.

### 4.5 Cross-restart correctness
- **Question:** On worker restart, what happens to in-flight codex children?
  - Today: they become orphans of PID 1, their output is lost, their DB rows linger until lease expiry.
- Options: on startup, read DB rows where `worker_id == self && status == running`, look up PIDs, either re-attach or terminate; persist PID in DB so re-attach is possible.

### 4.6 `ClaimNextTask` policy
- **Question:** When a lease expires, should the original worker be allowed to reclaim?
  - Today: yes (no `worker_id != ?` guard). That races with the still-running goroutine.
- Options: forbid self-reclaim; OR make self-reclaim a no-op that just refreshes the lease of the existing goroutine.

### 4.7 Observability
- **Question:** How do we detect (f)/(h) quickly without an external monitor script?
  - Today: we have one (`bpn9rkqga`) but `diagnose` misses heartbeat-only zombies.
- Options: expose `running_db_rows` and `live_codex_children` in `/metrics`; alert when they disagree for > 1 min; have the worker self-report this mismatch on each heartbeat.

## 5. Signals for the discussion

- Cheapest, highest-leverage change is probably §4.3 (reconcile slot count from DB). Everything else requires more design.
- §4.4 + §4.5 together give ops a sane story; right now we have neither.
- §4.1 is the deepest — a clean ownership primitive would simplify 4.2/4.3/4.5 at once but is the most invasive.

## 6. Out of scope for this discussion

- Review-agent assignment logic, GitHub label transitions, diagnose thresholds — these were the *symptoms* we chased before realizing the lifecycle itself is the problem.
