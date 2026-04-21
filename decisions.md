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
- `[L2]` Distributed worker task execution now delegates launcher/session/label-snapshot handling to `internal/worker.Executor`, so `cmd/worker.go` retains only transport concerns (heartbeat, release, submit/report) while embedded and remote paths share one execution core.
- `[L4][flagged]` Remote stale-inference remains injected as a task-level `RunSession` hook on the shared executor instead of moving the watchdog fully into `internal/worker/` in this slice; this keeps #143 shutdown/ownership-loss behavior intact while reducing duplicated session orchestration first.

## 2026-04-21

- `[L2]` Embedded serve-mode execution now lives in `internal/worker/embedded.go`; `cmd/serve.go` only wires coordinator services and starts an `EmbeddedWorker`, while the worker package owns the task loop, same-issue queue gate, close-skip logic, and reporting/audit completion flow.
- `[L4][flagged]` The embedded worker keeps an outer per-issue queue gate in addition to the executor's internal per-issue execution lock, because local regression tests showed executor-only locking allowed a queued same-issue task to start after the issue was closed; correctness of close/cancel semantics wins over a purer single-lock design in this slice.
- `[L2]` Shared session-event artifact writing is now exposed from `internal/worker/executor.go`, and repo docs were updated to point event-stream/session-artifact behavior at the worker package instead of the old inline `cmd/serve.go` path.
- `[L2]` The standalone worker now executes through `internal/worker/distributed.go`; `cmd/worker.go` keeps registration, poll, repo-binding, and concurrency plumbing, while heartbeat/release/watchdog/result submission are concentrated in the distributed worker service instead of another CLI-local execution function.
- `[L2]` GitHub CLI access for worker/reporter/router paths is now centralized in `internal/ghadapter/ghcli.go`, so new execution-path code stops scattering bespoke `gh issue/pr/api` shell-outs across packages and can share one mockable boundary.
- `[L4][flagged]` Runtime unification is currently delivered as a compatibility-first `internal/runtime` package that the worker core now targets, while `internal/launcher` still remains as a shim for untouched call sites; this favors finishing the worker-boundary migration safely before attempting a larger all-callsite rename/delete sweep.
- `[L2]` Worker-path session recording now goes through `internal/worker/session/{stream,recorder}.go`, replacing direct embedded/distributed calls to separate audit/eventlog writers with a single worker-scoped recording boundary.
- `[L2]` `internal/worker.Executor` now owns an explicit Start/Stop lifecycle and merges that lifecycle into each task run context, so embedded/distributed callers can stop in-flight shared execution without inventing another session-cancellation side channel.
- `[L2]` The runtime unification slice now extends beyond the worker core: serve/worker/run entrypoints plus router/reporter/ghadapter main-path types all target `internal/runtime`, while `internal/agent/bridge.go` was deleted and its translation logic was folded into `internal/launcher/agent_bridge.go` to remove the extra bridge layer before the final launcher shim cleanup.
- `[L2]` Production event hot paths no longer call `MustPayload(...)`; they use explicit payload encoding with safe fallback JSON instead, preferring degraded-but-reportable events over panic-driven worker loss.
