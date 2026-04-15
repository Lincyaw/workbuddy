# Decisions

## 2026-04-15

- `[L4]` Embedded worker scheduling now acquires the per-issue lock before taking a global parallel slot so queued work for one issue does not consume the shared concurrency budget and starve unrelated issues.
- `[L2]` Closed issues are tracked in-process so same-issue tasks that were already queued but not yet executing are skipped after the current task is cancelled, matching the issue-close semantics without broadening cross-issue cancellation.
