# Decisions

## 2026-04-15

- `[L2]` Post-run label validation now resolves the effective source state from the pre-run label snapshot before deriving allowed transitions, so a stale queued task state cannot misclassify a valid transition as unexpected.
- `[L4]` Embedded worker scheduling now acquires the per-issue lock before taking a global parallel slot so queued work for one issue does not consume the shared concurrency budget and starve unrelated issues.
- `[L2]` Closed issues are tracked in-process so same-issue tasks that were already queued but not yet executing are skipped after the current task is cancelled, matching the issue-close semantics without broadening cross-issue cancellation.
- `[L2]` Claude prompt executions now use a dedicated `stream-json` mapping path for Event Schema v1, while generic shell-backed `claude-code` commands remain on the minimal compatibility path so existing launcher tests and workflows do not regress.
- `[L2]` Issue dependency resolution now reads the settled per-cycle issue cache snapshot before dispatch, so dependency verdicts are computed from one coherent poll image instead of racing event emission order.
- `[L4]` Closed dependency satisfaction uses GitHub GraphQL `closedByPullRequestsReferences` through `gh api graphql`; this keeps the reader on the gh CLI boundary while giving deterministic “closed via linked PR” semantics without adding raw HTTP clients.

## 2026-04-16

- `[L2]` REQ-026 models the worker/coordinator payload around serialized `config.AgentConfig` plus `launcher.TaskContext`, so the remote worker executes through the existing launcher boundary instead of creating a second task schema just for HTTP mode.
- `[L2]` Task `ack` and `result` submission are retried with capped exponential backoff and treated as idempotent coordinator operations, which lets the worker survive coordinator restarts without re-running a completed launcher session.
- `[L3]` REQ-007 keeps GitHub integration on the `gh` CLI boundary by implementing the remote runner as a GitHub Actions adapter that shells out to `gh api`, then reconstructs `launcher.Result` from downloaded logs and artifacts instead of introducing a raw HTTP client beside the existing GH command model.
- `[L2]` REQ-007 now requests `return_run_details=true` on workflow dispatch and polls the returned run ID directly, removing the branch/time-window heuristic that could attach concurrent workflow_dispatch runs to the wrong remote session.
- `[L2]` Remote runner success now requires an ingested session artifact (`events-v1.jsonl` or `codex-exec.jsonl`); flattened Actions logs remain diagnostic output only and are no longer accepted as a fake session capture.
- `[L2]` `workbuddy recover --kill-zombies` only targets `codex` and `workbuddy serve` processes whose `/proc/<pid>/cwd` is inside the current repo's shared git root, which keeps recovery scoped to this repo instead of killing unrelated sessions on the host.

## 2026-04-17

- `[L2]` Worker worktrees keep the branch name stable per issue (`workbuddy/issue-N`) but now use task-scoped directory names, which preserves agent prompt continuity while preventing path reuse collisions across retries or worker restarts.
- `[L2]` `workspace.Manager.Create` now roots its best-effort `git worktree remove/prune` cleanup in the repo base directory, because relying on the process CWD broke same-issue worktree replacement outside repo-root call sites.
