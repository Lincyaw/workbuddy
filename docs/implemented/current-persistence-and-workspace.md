# Current Persistence and Workspace

状态：implemented

## 当前持久化事实

当前系统已经把核心运行数据落到 SQLite，包括：

- tasks
- workers
- issue cache
- transition counts
- event log
- agent sessions

核心代码：

- `internal/store/store.go`
- `internal/store/types.go`
- `internal/eventlog/log.go`
- `internal/registry/registry.go`

## 当前事件记录

除了业务表之外，系统还会把状态流转与异常写入 event log，供排障和测试使用。

相关代码：

- `internal/eventlog/log.go`
- `internal/statemachine/statemachine.go`

## 当前 Worker Registry

虽然运行形态是单进程，但当前仍然维护 registry 抽象，用于：

- 注册 worker
- 记录心跳
- 按 repo + role 查询候选 worker

这意味着未来切到远程 worker 时，并不需要完全推翻 registry 概念，只是 transport 层会变。

代码：

- `internal/registry/registry.go`
- `cmd/serve.go`
- `internal/router/router.go`

## 当前 Worktree 隔离

当前已经支持每个任务使用独立 git worktree：

- worktree 路径在 `.workbuddy/worktrees/`
- branch 名带 issue 编号与 task id
- task 结束后会尝试清理 worktree 和临时 branch
- 进程异常退出时支持 prune

代码：

- `internal/workspace/workspace.go`

## 当前意义

这部分能力已经是“实现事实”，不应再放在 roadmap 里当作未来计划：

- SQLite 不是计划，而是当前主存储
- session archive 不是计划，而是当前审计链路的一部分
- worktree isolation 不是计划，而是 router/worker 实际会用到的执行隔离手段
