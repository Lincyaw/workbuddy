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

- `serve`：Coordinator 继续直接挂载 `/sessions`，用于同进程部署下的本地 session 列表/详情。
- 所有拓扑的 Reporter comment 都统一输出 Coordinator `/workers/{worker_id}/sessions/{session_id}`。该入口与 Coordinator 其他 audit/task/metrics 路由共用同一个 Bearer auth surface，再由 Coordinator 代理到对应 Worker 的 management session viewer。
- Worker management surface 仍默认 loopback-only，可选共享 Bearer token；如果 Coordinator 与 Worker 分居不同主机，Worker 需要通过 `--mgmt-public-url` 注册一个 Coordinator 可达的管理入口（可以是直接监听地址、反向代理或隧道 URL），并且该入口必须复用 Coordinator 的 Bearer token。这保证了 split deployment 里也只有一套对外暴露的会话访问入口。

## 关键代码

- `cmd/serve.go`
- `cmd/coordinator.go`
- `cmd/worker.go`
- `internal/app/coordinator.go`
- `internal/worker/distributed.go`
- `internal/worker/executor.go`
- `internal/worker/reporting_helpers.go`
- `internal/reporter/reporter.go`
