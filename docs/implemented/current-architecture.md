# Current Architecture

状态：implemented

## 当前系统边界

当前只有**一套**运行时主链路：Worker 总是通过 HTTP 与 Coordinator 通信。
区别只在部署形态：

- `workbuddy serve`：在一个进程里同时启动 Coordinator HTTP server 和 Worker。
- `workbuddy coordinator` + `workbuddy worker`：把同一套代码拆成两个进程，可继续拆到不同机器。

主入口：

- `cmd/serve.go`
- `cmd/coordinator.go`
- `cmd/worker.go`
- `cmd/root.go`

## 当前主链路

无论如何启动，执行链都是：

1. Coordinator 加载 `.github/workbuddy/` 配置并打开 SQLite。
2. Coordinator 轮询 GitHub issue / PR，并驱动状态机。
3. 状态机通过 Router 持久化 task。
4. Worker 通过 `GET /api/v1/tasks/poll` 领取任务。
5. Worker 调用 runtime 的 `Start(...) -> Session.Run(...)` 执行 agent。
6. Worker 做 label snapshot / label validation / reporter comment / session audit。
7. Worker 通过 `/api/v1/tasks/:id/result` 或 `/release` 回写结果。
8. Coordinator 持久化事件、更新 task 状态，并暴露 audit / metrics / session surface。

关键代码：

- `cmd/serve.go`
- `cmd/coordinator.go`
- `cmd/worker.go`
- `internal/app/coordinator.go`
- `internal/worker/distributed.go`
- `internal/worker/executor.go`

## 当前拓扑

```text
serve
  -> start coordinator HTTP surface
  -> start worker (pointed at localhost coordinator)

split deployment
  coordinator process
    -> poller / state machine / HTTP API
  worker process
    -> long-poll / execute / submit result
```

这意味着：

- 已不存在进程内 channel worker transport。
- comment payload、claim verification、needs-human comment、submit-result fallback 都走同一个 worker 实现。
- `serve` 与 split deployment 的差异只剩下 listen/auth 配置和是否拆进程。

## 当前 HTTP / Auth 事实

- Coordinator 默认监听 loopback 地址。
- 非 `/health` 路由在开启 `--auth` 时都经过同一个 `WrapAuth` Bearer middleware。
- `serve` 复用 Coordinator 的 HTTP surface，而不是维护第二套无认证 mux。
- Worker management server 仍默认 loopback-only，并支持可选共享 Bearer token；split-host 部署通过 `--mgmt-public-url` 把一个 Coordinator 可达的 management base URL 注册进 worker metadata。
- GitHub issue comments 现在统一链接 Coordinator `/workers/{worker_id}/sessions/{session_id}`；Coordinator 在同一个 auth surface 下代理到 Worker session viewer，因此 `serve` 和 split deployment 的 comment payload 保持一致。
