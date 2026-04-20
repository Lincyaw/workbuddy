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
- `[L2]` Remote runner success now requires an ingested unified session artifact (`events-v1.jsonl`); flattened Actions logs remain diagnostic output only and are no longer accepted as a fake session capture.
- `[L2]` `workbuddy recover --kill-zombies` only targets `codex` and `workbuddy serve` processes whose `/proc/<pid>/cwd` is inside the current repo's shared git root, which keeps recovery scoped to this repo instead of killing unrelated sessions on the host.

## 2026-04-18

- `[L3]` The Codex installer now syncs `plugins/workbuddy/skills` into `~/.codex/skills` instead of relying on repo-local plugin marketplace paths, because local verification confirmed the file-based skills directory is the only runtime shape we can confidently target from this repository alone.
- `[L2]` The installer records a workbuddy-specific state file at `~/.codex/.workbuddy-installed-skills.json` and treats it as the managed set, which makes repeated installs idempotent and lets upgrades add, overwrite, or prune only workbuddy-owned skills without touching unrelated local skills.
- `[L4]` `WORKBUDDY_KEEP_REMOVED=1` preserves retired upstream workbuddy skills locally but keeps them in the managed set, so a later default sync can still prune them; this favors reversible operator control over a simpler but lossy state model.
- `[L2]` Deployment state is now persisted as a per-scope manifest (`$XDG_CONFIG_HOME/workbuddy/deployments` for user scope, `/etc/workbuddy/deployments` for system scope), so `workbuddy deploy install`, `redeploy`, and `upgrade` can share one recorded service definition instead of forcing operators to re-enter paths and command arguments.
- `[L4]` The deploy surface records the `workbuddy` runtime arguments after `--` and defaults to a valid `serve` invocation, which keeps the CLI compact while still covering `serve`, `coordinator`, and `worker` deployments without separate systemd wrappers for each topology.

## 2026-04-20

- `[L3]` Issue/PR #143/#144 assumed Codex should be driven through `codex mcp-server`, but local schema generation from `codex app-server` and live handshake validation showed the supported control plane is the app-server JSON-RPC protocol (`initialize`, `thread/start`, `turn/start`, `turn/interrupt`, server requests, framed notifications), so the backend was rewritten around that contract instead of retrofitting MCP semantics.
- `[L4][flagged]` The new Codex app-server backend currently spawns one app-server child per agent session instead of multiplexing a worker-wide singleton, because workbuddy depends on per-agent scoped env/token isolation and the app-server protocol does not expose per-thread environment injection; this favors correctness and simpler cancellation over the thread-resume benefits of a shared process.
- `[L2]` After verifying the app-server path end-to-end, the legacy local Codex launcher and its temporary compatibility switch were removed entirely, so `runtime: codex` now resolves directly to the JSON-RPC backend and CLI/session metadata no longer report a stale legacy runtime name.
