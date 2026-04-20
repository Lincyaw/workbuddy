# Remote Runner (GitHub Actions)

状态：implemented

## Agent Config

Agent frontmatter 现在支持独立的 `runner` 选择：

- `runner: local`：沿用当前 worker 机器上的本地 runtime
- `runner: github-actions`：由 launcher 触发 GitHub Actions workflow，再回收日志和 session artifact

当 `runner: github-actions` 时，还支持可选的 `github_actions` 配置块：

```yaml
runner: github-actions
github_actions:
  workflow: workbuddy-remote-runner.yml
  ref: main
  poll_interval: 5s
```

默认值：

- `workflow`: `workbuddy-remote-runner.yml`
- `poll_interval`: `5s`
- `ref`: 若未显式配置，则从当前 repo worktree 的 `git rev-parse --abbrev-ref HEAD` 推断

代码：

- `internal/config/types.go`
- `internal/config/loader.go`

## Launcher Behavior

launcher 在 `agent.Runner == github-actions` 时不会走本地 runtime 子进程，
而是切到 `internal/launcher/runners/gha/`：

1. 通过 `gh api` 向
   `repos/{repo}/actions/workflows/{workflow}/dispatches`
   发送 `workflow_dispatch`
2. 输入包含：
   - `repo`
   - `issue`
   - `agent`
   - `session_id`
3. dispatch 时要求 `return_run_details=true`，直接拿到本次触发对应的 run ID
4. run 完成后下载：
   - run logs zip
   - run artifacts zip
5. 若 artifact 内存在 `events-v1.jsonl`，将其作为 `launcher.Result.SessionPath`
6. 若 artifact 内存在 `workbuddy-result.json`，将其反序列化回 `launcher.Result`

代码：

- `internal/launcher/launcher.go`
- `internal/launcher/gha_runner.go`
- `internal/launcher/runners/gha/gha.go`

## Artifact Contract

远程 workflow 至少应上传一个包含 session capture 的 artifact。当前 adapter 识别这些文件：

- `events-v1.jsonl`
  - 优先作为 canonical session artifact
  - 必须存在
- `workbuddy-result.json`
  - 可选；用于回传 `exit_code`、`last_message`、`meta`、`session_ref`、`token_usage`

如果 artifact 中没有 `events-v1.jsonl`，remote runner 会直接返回错误，而不是把纯文本日志伪装成 session artifact。

`workbuddy-result.json` 的最小示例：

```json
{
  "exit_code": 0,
  "last_message": "remote run completed",
  "meta": {
    "runner": "github-actions"
  },
  "session_path": "events-v1.jsonl"
}
```

## Example Workflow

仓库中提供了示例 workflow：

- `.github/workflows/workbuddy-remote-runner.yml`

它演示了：

- `workflow_dispatch` 输入合同
- `run-name` 中包含 `agent` / `issue` / `session_id`，方便人工排查
- 生成 `.workbuddy/sessions/<session_id>/` 目录
- 上传 `workbuddy-session` artifact

示例里的执行步骤是占位实现，预期仓库按自己的 agent 执行入口替换。
只要最终产出满足上面的 artifact contract，launcher 就能完成日志和 session ingest。

## Tests

`internal/launcher/runners/gha/gha_test.go` 使用 fake `gh` 命令模拟 Actions API，
覆盖了：

- dispatch
- dispatch 返回精确 run ID 后的 poll
- log zip 下载
- artifact zip 下载与解压
- canonical session artifact 识别
- 缺失 session artifact 时失败
