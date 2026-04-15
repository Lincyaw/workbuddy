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

## 当前重试与失败边界

当前 `max_retries` 闭环只有“检测 + 计数 + 记录 intent”，还不是 Go 侧完整的失败标签编排能力。

- `internal/statemachine/statemachine.go` 会根据回退边历史调用 `internal/store/store.go` 的 `IncrementTransition` / `QueryTransitionCounts`，把每个 issue 的回退次数持久化到 SQLite `transition_counts` 表。
- 当回退次数达到 `workflow.max_retries` 时，state machine 只记录 `TypeCycleLimitReached` 和 `TypeTransitionToFailed` 事件，并停止派发这次回退；这里记录的是“应该失败 / 需要人工介入”的 intent。
- Go 侧不会直接写 `status:failed` 或 `needs-human` label。当前 GH call boundary 仍然是 agent 通过 `gh issue edit` 负责 label 写回，Go 侧只做检测、审计和派发。

这也意味着当前 retry/failure 语义仍然有明确边界：

- 去重只覆盖单个 poll 周期内的相同事件，`ResetDedup()` 会在下一轮 poll 前清空内存去重集合，所以 retry 是否再次触发仍然受 poll 周期切分影响。
- state entry 在 label 变化时会主动清理 stale inflight；`MarkAgentCompleted` 和 `CheckStuck` 再依据“任务完成时看到的 labels”判断后续事件，label 变化和 task 完成之间仍然存在竞态窗口。
- back-edge 计数持久化与 GitHub label 写回不是同一个原子动作，所以 `transition_counts` 已更新，并不等于 `status:failed` / `needs-human` 已经被外部执行端成功写回。
