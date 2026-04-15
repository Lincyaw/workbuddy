# Current Architecture

状态：implemented

## 当前系统边界

当前真正可运行的形态是 `workbuddy serve` 单进程模式，而不是旧文档里描述的 coordinator/worker 分布式部署。

主入口：

- `cmd/serve.go`
- `cmd/root.go`

当前 CLI 事实：

- 已实现命令：`workbuddy serve`
- 未实现旧设计中提到的 `coordinator`、`worker`、`init`、`status`、`run`、`validate`、`logs`

## 当前主链路

`serve` 启动后会组装下面这些模块：

1. 加载 `.github/workbuddy/` 配置。
2. 打开 SQLite：`.workbuddy/workbuddy.db`。
3. 初始化 event log、worker registry、router、state machine、launcher、reporter、audit、web UI。
4. 用 `gh` CLI 轮询 GitHub issue/PR。
5. Poller 事件进入 state machine。
6. 进入带 `agent` 的状态时，state machine 发出 dispatch request。
7. Router 创建 task，并通过 Go channel 发送给进程内 worker。
8. Worker 调用 launcher 运行 agent 子进程。
9. 结果写回 reporter、audit、store，并通过 `/sessions` 可查看。

关键代码：

- `cmd/serve.go`
- `internal/poller/poller.go`
- `internal/statemachine/statemachine.go`
- `internal/router/router.go`

## 当前拓扑

```text
workbuddy serve
  -> Poller
  -> StateMachine
  -> Router
  -> embedded worker
  -> Launcher runtime
  -> Reporter / Audit / Web UI
```

这意味着：

- 当前不是 HTTP 长轮询 worker 通信。
- 当前不是多 repo coordinator 中心调度。
- 当前不是远端 worker 注册后领取任务。
- 当前 transport 固定是进程内 channel。

## 当前 GitHub 交互方式

当前所有 GitHub 读写都依赖 `gh` CLI，而不是 Go 内嵌 REST client：

- Poller 读 issue/pr：`cmd/serve.go`
- Router 拉 issue 详情：`internal/router/router.go`
- Reporter 写 issue comment：`internal/reporter/reporter.go`
- Agent 自己在 prompt/command 中调用 `gh issue edit` 推进 label

## 当前状态推进原则

当前系统更接近“Agent-as-Router”：

- Go 侧负责检测 label 变化、识别当前状态、派发 agent。
- Agent 子进程负责实际改 label。
- Go 侧并不直接代替 agent 修改 issue label。

这和旧设计的“中心状态机统一管理所有迁移”不是同一件事，后续如要调整，必须先改 `docs/mismatch/` 中对应差异文档。
