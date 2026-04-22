# Distributed Topology and CLI

状态：implemented (v0.2.0)

## 当前拓扑

workbuddy 支持两种部署形态：

### 单机模式 (v0.1.0+)

```
workbuddy serve --port 8080 --roles dev
```

Coordinator 和 Worker 在同一进程内，通过 Go channel 通信。

### 分布式模式 (v0.2.0+)

```
# 机器 A
workbuddy coordinator --port 8080

# 机器 B
workbuddy worker --coordinator http://A:8080 --token <secret> --role dev --repos owner/repo=/srv/workbuddy-worker
```

- Coordinator 负责：GitHub Poller、状态机、任务路由、HTTP API、审计
- Worker 负责：向 Coordinator 注册、长轮询领取任务、执行 agent 子进程、提交结果
- 通信方式：HTTP 长轮询（`GET /api/v1/tasks/poll`，无任务时挂起最多 timeout 秒）
- 认证：共享密钥，`Authorization: Bearer <token>`（REQ-029）

## CLI 命令列表

所有命令均已实现并通过测试。

| 命令 | 说明 | REQ | 版本 |
| --- | --- | --- | --- |
| `workbuddy serve` | 单进程模式（Coordinator + Worker） | REQ-017 | v0.1.0 |
| `workbuddy coordinator` | 仅运行 Coordinator，任务通过 HTTP API 分发 | REQ-027 | v0.2.0 |
| `workbuddy worker` | 仅运行 Worker，连接远程 Coordinator | REQ-028 | v0.2.0 |
| `workbuddy init` | 初始化仓库配置和运行时目录 | REQ-018 | v0.2.0 |
| `workbuddy status` | 查看 issue 状态、task 队列、事件、阻塞等待 | REQ-019, 033, 035, 036 | v0.2.0 |
| `workbuddy run` | 直接启动 runtime session（跳过 poller/状态机） | REQ-020 | v0.2.0 |
| `workbuddy validate` | 校验配置文件完整性和交叉引用 | REQ-021 | v0.2.0 |
| `workbuddy logs` | 查看 session 归档日志和 artifact | REQ-022 | v0.2.0 |
| `workbuddy cache invalidate` | 清除 issue 缓存强制重新评估（`cache-invalidate` 为 deprecated alias） | REQ-034, 061 | v0.2.0 |
| `workbuddy issue restart` | 清除 issue cache / dependency state / issue claim，强制下一 poll 重新派发（`admin restart-issue` 为 deprecated alias） | REQ-060, 061 | v0.2.0 |
| `workbuddy diagnose` | 自动诊断 pipeline 问题（stuck/orphaned/repeated failure） | REQ-037 | v0.2.0 |
| `workbuddy recover` | 重启恢复（清理僵尸进程、重置运行时状态） | REQ-032 | v0.2.0 |
| `workbuddy deploy` | 安装当前 binary、写入 systemd unit，并支持后续 redeploy/upgrade/start/stop/delete | main | main |
| `workbuddy worker unregister` | 从 Coordinator 注销指定 Worker | REQ-052 | v0.2.0 |

## Coordinator HTTP API

| 端点 | 方法 | 说明 |
| --- | --- | --- |
| `/api/v1/workers/register` | POST | Worker 注册 |
| `/api/v1/workers/:id` | DELETE | Worker 注销（从 registry 永久删除） |
| `/api/v1/tasks/poll` | GET | 长轮询领取任务 |
| `/api/v1/tasks/:id/result` | POST | 提交执行结果 |
| `/api/v1/tasks/:id/heartbeat` | POST | 心跳 |
| `/api/v1/tasks/:id/release` | POST | 释放已 claim 的任务 |
| `/health` | GET | 健康检查 |

### Worker 注销语义

- `DELETE /api/v1/workers/:id` 会从 registry 中**永久删除** worker 记录，而非仅标记为 offline。
- 如果该 worker 当前持有运行中的任务（`task_queue.status = 'running'`），注销会被拒绝并返回 `409 Conflict`（保守策略）。
- 注销后，同一 worker ID 再次 poll/heartbeat 会收到 `400 unknown worker` 错误，需要重新注册。

详见 `docs/api/coordinator-v1.yaml`。

## Deploy 命令

`workbuddy deploy` 记录一份 scope-aware deployment manifest，使 install、redeploy、
upgrade、start/stop/delete 可以围绕同一份定义重复执行，而不是每次手工重写 systemd 参数。

- `workbuddy deploy install`：把当前正在执行的 `workbuddy` binary 安装到目标路径；可选写入 user/system scope 的 systemd unit。
- `workbuddy deploy redeploy`：读取 manifest，重新安装当前 binary，刷新 unit，并重启 service。
- `workbuddy deploy upgrade`：从 GitHub Releases 下载指定版本（或 latest）的归档，替换部署 binary，并重启 service。
- `workbuddy deploy stop`：停止已托管的 systemd service，但保留 unit、enable 状态和 deployment manifest，方便后续恢复。
- `workbuddy deploy start`：根据已有 manifest 重新加载并启动已托管的 systemd service，无需重装 binary。
- `workbuddy deploy delete`：删除 deployment manifest 和 systemd unit，并停止 service；已安装的 binary 会保留在原路径。
- `--scope user|system`：分别使用 XDG user 目录或 `/etc` 下的 systemd / manifest 路径，便于开发机和系统服务两种场景。
- `install` 支持在 `--` 后传递任意 `workbuddy` 子命令参数，因此同一套 deploy surface 可以覆盖 `serve`、`coordinator`、`worker` 等运行形态。

典型安装形态：

- 单机部署：`workbuddy deploy install --scope user --systemd`（默认记录 `serve`）
- 分布式 Coordinator：`workbuddy deploy install --scope system --systemd -- coordinator ...`
- 分布式 Worker：`workbuddy deploy install --scope system --systemd -- worker --coordinator ... --token ...`

也就是说，Coordinator / Worker 没有单独的 deploy 子命令；二者都是通过
`deploy install -- <runtime args>` 明确指定运行时角色。

## 主要代码

- `cmd/coordinator.go` — Coordinator 命令
- `cmd/deploy.go` — 部署 / systemd / upgrade 命令
- `cmd/worker.go` — Worker 命令
- `internal/coordinator/http/handler.go` — Coordinator HTTP API handler (includes auth middleware)
- `internal/workerclient/client.go` — Worker HTTP client
- `internal/store/worker_tokens.go` — Token 管理
