# Decisions

## 2026-04-15

- `[L2]` Post-run label validation now resolves the effective source state from the pre-run label snapshot before deriving allowed transitions, so a stale queued task state cannot misclassify a valid transition as unexpected.
- `[L4]` Embedded worker scheduling now acquires the per-issue lock before taking a global parallel slot so queued work for one issue does not consume the shared concurrency budget and starve unrelated issues.
- `[L2]` Closed issues are tracked in-process so same-issue tasks that were already queued but not yet executing are skipped after the current task is cancelled, matching the issue-close semantics without broadening cross-issue cancellation.
- `[L2]` Claude prompt executions now use a dedicated `stream-json` mapping path for Event Schema v1, while generic shell-backed `claude-code` commands remain on the minimal compatibility path so existing launcher tests and workflows do not regress.
