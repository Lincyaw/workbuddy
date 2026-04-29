# Worker Execution Boundary

状态：implemented

## 结论

当前只有一套 Worker 执行实现：`internal/worker/distributed.go`。

- `workbuddy worker` 直接运行这条路径。
- `workbuddy serve` 也运行这条路径，只是把 Coordinator 和 Worker 放进同一个进程里，并让 Worker 指向 `127.0.0.1` 上的 Coordinator。

## 当前职责切分

### Coordinator

- 维护 repo registration、worker registration、task queue、claim lease、audit / metrics HTTP surface。
- 负责 `/api/v1/tasks/poll`、`/result`、`/heartbeat`、`/release` 协议。

### Worker transport (`internal/worker/distributed.go`)

- 长轮询领取 task。
- 运行 heartbeat / release / submit-result 生命周期。
- 调用 Reporter 产出 started / needs-human / verified comments。
- 进行 label snapshot、label validation、claim verification、session audit。
- 在 submit-result 失败时通过 typed reporter sync-failure 输入产出统一 comment，而不是拼接 ad-hoc 字符串。

### Worker execution core (`internal/worker/executor.go`)

- 创建 / 复用 runtime session。
- 管理 worktree 生命周期。
- 采集 Event Schema v1 artifacts。
- 统一 pre/post label snapshot。

## Session viewer / audit

- `serve`：Coordinator 直接挂载 `/sessions`，因为 Coordinator 与 Worker 共用同一个 DB / sessions 目录；Reporter comment 里的 session 链接也指向这个 surface。
- split deployment：Worker management surface 提供 loopback-only 的 session viewer，可选 Bearer token 保护；Coordinator 继续提供统一的 audit / task / metrics surface。为了避免把 GitHub comments 绑到 worker-local 监听地址，split deployment 的 Reporter comment 不直接输出 session viewer URL。

## 关键代码

- `cmd/serve.go`
- `cmd/coordinator.go`
- `cmd/worker.go`
- `internal/app/coordinator.go`
- `internal/worker/distributed.go`
- `internal/worker/executor.go`
- `internal/worker/reporting_helpers.go`
- `internal/reporter/reporter.go`
