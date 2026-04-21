# Worker Execution Boundary Extraction

状态：planned / in progress (#146)

## 当前落地进度（2026-04-21）

- 已落地：`internal/worker/task.go` + `internal/worker/executor.go` 已成为 shared execution core；embedded 与 distributed 都复用同一个 launcher/session/label-snapshot 执行主链。
- 已落地：`internal/worker/embedded.go` 已接管 serve 模式的 embedded worker loop、per-issue 队列串行化、closed-issue queued-task skip，以及 task-level panic recovery。
- 已落地：`internal/worker/distributed.go` 已接管 remote worker 的 heartbeat / release / submit / watchdog 执行边界，`cmd/worker.go` 只保留 CLI、注册、repo 绑定与并发调度。
- 已落地：`internal/ghadapter/ghcli.go` 已成为 worker/reporter/router 共享的 gh CLI 读写边界；新的 worker execution path 不再直接在路由/报告逻辑里散落 `os/exec gh ...`。
- 已落地：`internal/worker/session/stream.go` + `internal/worker/session/recorder.go` 已成为 worker 路径里的统一 session event stream / audit-event write boundary。
- 已落地：worktree setup / cleanup 已收口到 `internal/worker/executor.go`；`internal/router/router.go` 不再在 dispatch 时 eager-create worktree，embedded / distributed 都通过 shared executor 进入同一条工作区隔离主链。
- 已落地：session / event storage 退化会显式写入 session `health.json`，并同步暴露到 `runtime.Result.Meta`，不再只停留在 stderr / log。
- 已落地：`internal/runtime/runtime.go` 已提供新的 canonical runtime/session package name，worker execution core、serve/worker/run 主路径、router/reporter/ghadapter 的 shared runtime/result 上下文现在都优先面向 `internal/runtime`，`internal/launcher` 仍作为兼容 shim 存在。
- 已落地：`cmd/serve.go` 不再内联 `runEmbeddedWorker(...)` / `executeTask(...)` / `streamSessionEvents(...)`。
- 已落地：`internal/agent/bridge.go` 已删除，agent-session → launcher event/result translation 直接收口到 `internal/launcher/agent_bridge.go`。
- 已落地：`internal/worker/executor.go` 现在有显式 `Start()` / `Stop()` lifecycle，且 worker package 已补 direct executor 单元测试。
- 已落地：runtime event hot path 已移除生产调用里的 `MustPayload(...)` panic 依赖，改为显式编码分支。
- 仍未完成：`internal/launcher/` 的彻底退役还没做完；当前是“新边界已落地，旧包仍保留兼容 shim”的状态。

## 背景

当前仓库仍然有两条 worker 入口，但执行核心已经开始收口：

- serve 拓扑的 embedded worker：通过进程内 channel 接收 `router.WorkerTask`，现已迁移到 `internal/worker/embedded.go` 驱动。
- distributed worker：通过 `internal/workerclient.Client` 长轮询 Coordinator，runtime/session 主链已复用 shared executor，但 heartbeat / release / submit / watchdog 等 transport 逻辑仍在 `cmd/worker.go`。

这导致同一类语义分散在多个入口：

- 执行生命周期被 CLI 命令文件持有，而不是被 runtime/service package 持有。
- embedded 与 distributed 在 start-failure、infra-failure、event draining、worktree cleanup 上各自演化，容易继续漂移。
- `internal/launcher/` 同时承担 runtime registry、agent bridge、infra failure 分类、event encoding、session process 等多种职责。
- GitHub 读取仍散落在 `cmd/serve.go`、`internal/router/router.go`、`internal/reporter/reporter.go` 等位置。
- `internal/audit` 与 `internal/eventlog` 的 session 记录路径并不统一，退化行为也不一致。

`#146` 的目标不是做一次“大重写”，而是先把“任务执行边界”从 `cmd/*` 中抽出来，让两种拓扑共享一套 canonical worker execution core。

## 设计目标

1. 提取一个可复用的 `internal/worker/` 执行层，统一 embedded / distributed 两种 worker 的任务生命周期。
2. 让 `cmd/serve.go` 只保留 coordinator 装配、embedded transport 接线、HTTP / poller / state machine 启动。
3. 让 `cmd/worker.go` 只保留 distributed transport、worker registration / poll / submit 等拓扑逻辑。
4. 收敛 `agent.Backend` 与 `launcher.Runtime` 两套重叠抽象，形成单一 runtime/session 接口。
5. 把 GitHub 读边界和 session artifact 写边界收口成显式依赖，而不是散落在执行流程里临时 shell out。

## 非目标

这份设计暂不覆盖：

- poller 的原子快照改造
- worker token / coordinator auth model 统一
- operator UX / session proxy 的统一外显
- store 分层或 router 的完整重构

这些属于 `#145` 其它子问题；这里只处理“worker execution core”边界。

## 当前代码边界

### embedded path

当前 embedded worker 主路径位于：

- `internal/worker/embedded.go` `EmbeddedWorker.Run(...)`
- `internal/worker/embedded.go` `EmbeddedWorker.ExecuteTask(...)`
- `internal/worker/executor.go` `Executor.Execute(...)`
- `internal/worker/executor.go` `streamSessionEvents(...)`

其中 `internal/worker/embedded.go` 仍持有：

- embedded 专用的 outer per-issue queue gate（用于 closed-issue queued-task skip 语义）
- `runningTasks`
- reporter / audit / state machine completion

而共享 `Executor` 已持有：

- executor-level per-issue execution lock
- worktree setup / cleanup
- `launcher.Start(...)` + `session.Run(...)`
- label snapshot / validation
- canonical session event stream

### distributed path

当前 distributed worker 主路径位于：

- `internal/worker/distributed.go` `DistributedWorker.ExecuteTask(...)`
- `internal/worker/distributed.go` `runRemoteSessionWithWatchdog(...)`

它当前主要仍持有：

- heartbeat / release lifecycle
- worktree setup / cleanup
- stale-inference watchdog / event drain hook
- result classification / submit
- reporter start / verified report

### runtime boundary

当前 runtime 相关能力散落在：

- `internal/launcher/types.go`：`Runtime` / `Session` / `Result`
- `internal/launcher/agent_bridge.go`：当前直接持有 agent-session 到 launcher/runtime Session 的翻译逻辑
- `internal/agent/claude/backend.go`
- `internal/agent/codex/backend.go`
- `internal/launcher/gha_runner.go`
- `internal/launcher/infra_failure.go`
- `internal/launcher/events/*`

这说明“统一抽象”已经部分存在，但调用路径仍然围绕历史 bridge 组织，尚未形成更清晰的 `runtime` package 边界。

## 目标结构

```text
cmd/serve.go
  -> build coordinator runtime
  -> create embedded worker transport
  -> start embedded worker service

cmd/worker.go
  -> create distributed transport (workerclient)
  -> start distributed worker service

internal/worker/
  executor.go
  embedded.go
  distributed.go
  task.go
  result.go
  session/
    recorder.go
    stream.go

internal/runtime/
  runtime.go
  registry.go
  claude.go
  codex.go
  gha.go
  infra.go
  events/

internal/github/
  adapter.go
  ghcli.go
```

说明：

- `internal/worker/` 拥有任务执行生命周期，但不拥有 transport 协议。
- `internal/runtime/` 拥有 runtime/session 抽象与 backend 实现。
- `internal/github/` 是统一的 `gh` CLI 访问边界，供 router / worker / reporter / poller 逐步复用。
- `internal/launcher/` 在迁移完成后应被删除，或退化为临时兼容层。

## 核心契约

### worker.Task

`worker.Task` 是 canonical 执行输入，字段应覆盖 embedded `router.WorkerTask` 与 distributed `workerclient.Task` 的公共子集：

- `TaskID`
- `Repo`
- `IssueNum`
- `AgentName`
- `Workflow`
- `State`
- `Agent` / runtime config snapshot
- `Context` / session seed
- `Attempt`
- `TransportMeta`（仅 transport 自身使用，不参与执行判定）

embedded / distributed 入口都只负责把各自 transport payload 适配成 `worker.Task`。

### worker.Result

`worker.Result` 是 canonical 执行输出，至少包含：

- `Status`
- `ExitCode`
- `CurrentLabels`
- `InfraFailure`
- `InfraReason`
- `SessionID`
- `SessionArtifactPath`
- `RawSessionArtifactPath`
- `StartedAt` / `FinishedAt`
- `Attempt`

transport 层只消费这个结果做 topology-specific ack / submit / state-machine completion。

### worker.Executor

`Executor` 对外只暴露两类能力：

- `Start()` / `Stop()`：启动和停止内部长期 goroutine（如 session stream drain / background cleanup）
- `Execute(ctx, task)`：执行单个 `worker.Task` 并返回 `worker.Result`

`Executor` 内部拥有：

- per-issue execution lock
- worktree setup / cleanup
- runtime registry lookup
- session start / run / close
- label snapshot / validation
- session record / event stream
- start-failure / nil-result / timeout / infra-failure 统一分类

## 生命周期分层

### EmbeddedWorker

`EmbeddedWorker` 负责：

- 从 channel 读取任务
- 做 goroutine / panic recovery / bounded parallelism
- 调用 `Executor.Execute(...)`
- 把执行结果回写给当前单进程 runtime（例如 state machine completion / task notifications / reporter hooks）

它不再自己实现 worktree/runtime/session 细节。

### DistributedWorker

`DistributedWorker` 负责：

- poll / heartbeat / release / submit result
- worker registration / shutdown 协调
- ownership-lost 等 transport 错误处理
- 把领取到的任务适配成 `worker.Task`
- 调用同一个 `Executor.Execute(...)`

它不再自己实现 runtime/session/audit 主逻辑。

## GitHub 边界

迁移时需要同时引入统一的 GitHub adapter，否则 `Executor` 很容易再次把 `os/exec gh ...` 写进内部，继续扩大耦合。

当前这一步已经完成第一阶段：worker / reporter / router 新代码路径统一走 `internal/ghadapter/ghcli.go`。

第一阶段至少要抽出 worker 执行路径会用到的读能力：

- 读取 issue body / labels / comments / related PRs
- 读取 pre/post label snapshot

第二阶段再把 router / reporter / poller 逐步迁移到同一 adapter。

注意：Reporter 是否仍保留自己的“写 comment”实现可以分阶段处理，但新的执行核心不应直接 shell out 调 `gh`。

## Session 记录边界

当前 eventlog / audit 都能接触 session 结果，但落盘路径和降级策略不统一。

目标态：

- `internal/worker/session/stream.go` 只负责把 runtime event stream 安全地写成 canonical artifact。
- `internal/worker/session/recorder.go` 负责把 session artifact、token usage、degraded storage 状态统一封装成单一记录入口。
- 存储退化要转化成显式 health / result metadata，而不是只打 stderr。

其中前两项已经在 worker path 落地；更广泛的 eventlog/audit 读侧整合仍可在后续 issue 继续收口。

## 迁移切片

建议按下面顺序拆分：

### Slice 1：抽象与执行核心骨架（已落地）

- 已定义 `worker.Task` / `worker.Execution` / `worker.Executor`
- shared executor 已接管 launcher/session/label snapshot/event stream 主链
- distributed transport 已先行复用 shared executor，但 stale-inference watchdog 仍通过 task hook 注入

### Slice 2：embedded worker 接线（已落地本轮）

- 已新增 `internal/worker/embedded.go`
- `cmd/serve.go` 现只创建 transport channel + start embedded worker
- inline `runEmbeddedWorker` / `runWorkerTask` / `executeTask` / `streamSessionEvents` 已从 `cmd/serve.go` 移出

### Slice 3：distributed worker 复用执行核心（已落地本轮）

- 已新增 `internal/worker/distributed.go`
- `cmd/worker.go` 的远程执行主体已收缩为 shared `DistributedWorker` 调用
- heartbeat / release / ownership-lost 仍作为 distributed transport concern 保留在 worker service 边界内

### Slice 4：runtime 包统一

- 把 `launcher.Runtime` / `launcher.Session` / infra-failure / registry 搬到 `internal/runtime/`
- 删除 `internal/launcher/agent_bridge.go`
- Claude / Codex / GHA 实现改为直接满足统一 `runtime.Runtime`
- `MustPayload` 改成 error-returning encoder，热路径不再 panic

### Slice 5：GitHub adapter + session recorder 收口

- 抽 `internal/github/`
- 让 executor / router / reporter 不再直接 `os/exec gh ...`
- 合并 audit/eventlog 的 session 记录入口

## 验收策略

设计完成后的实现至少要验证：

- `cmd/serve.go` 显著收缩到“组装 + transport 接线”级别
- embedded / distributed 两条路径共享同一个 `Executor`
- runtime 启动失败、session nil result、timeout、ownership-lost 有统一分类边界
- 长生命周期 goroutine 都带 panic recovery，且可被 `Stop()` 约束退出
- `go build ./...`、`go vet ./...`、`go test ./...` 全绿

## 与 Issue 的对应关系

- Parent debt umbrella: `#145`
- 执行核心提取：`#146`
- 本文档是 `#146` 的 repo 内设计锚点；后续 sub-issues 应按 migration slice 继续拆分，而不是让单个 issue 一次性同时改 transport、runtime、GitHub adapter、session recorder。
