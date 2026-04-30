# Pipeline Observability and Diagnosis

状态：implemented (v0.2.0)

## 概述

将原 `pipeline-monitor` skill 中的高频手动操作下沉为 CLI 一等公民，提供结构化的 pipeline 观测和自动诊断能力。对应 REQ-033 ~ REQ-037。

当前还补充了进程内 operator detector：

- `internal/operator/detector.go` 周期扫描 `task_queue`、`issue_cache`、`workers`
- 命中规则后把结构化 alert 持久化到 `events(type=alert)`
- 同时写入 `~/.workbuddy/operator/inbox/*.json`
- 可通过 `GET /api/v1/alerts?since=<rfc3339>&severity=warn` 查看最近告警

## CLI 工具

### status --tasks（REQ-033）

查看当前 task queue 中的运行/等待/失败任务：

```bash
workbuddy status --tasks [--repo R] [--json]
```

输出列：REPO, ISSUE, AGENT, STATUS, WORKER, UPDATED。后端通过 `GET /tasks?repo=X&status=Y` HTTP 端点查询。

### status --events（REQ-035）

查询事件日志，替代 `tail serve.log | grep`：

```bash
workbuddy status --events [--repo R] [--since 1h] [--type stuck_detected] [--json]
```

输出列：TIME, TYPE, ISSUE, PAYLOAD。默认按时间倒序，最多 50 条。

### status --watch（REQ-036）

阻塞等待下一个 task 完成，基于 SSE（Server-Sent Events）：

```bash
workbuddy status --watch [--repo R] [--issue N] [--timeout 30m]
```

退出码约定：completed=0, failed=1, timeout=2, watch 自身超时=3。

serve 进程内 `internal/tasknotify/hub.go` 实现 Publish/Subscribe，通过 `GET /tasks/watch` SSE 端点推送 task 完成事件。

### cache invalidate（REQ-034, REQ-061）

手动清除 issue 缓存，强制 poller 重新评估：

```bash
workbuddy cache invalidate --repo OWNER/NAME --issue 47,48,49
```

- 删除 `issue_cache` 行
- 重置 `issue_dependency_state`（清除 verdict，强制重评估）
- 记录 `cache_invalidated` 事件
- 直连 SQLite，不依赖 serve 进程
- `workbuddy cache-invalidate ...` 仍可用，但会在 stderr 打印 deprecation warning

### issue restart（REQ-060, REQ-061, REQ-114）

显式重启单个 issue 的调度状态，解决“标签没变但就是不再派发”的恢复场景：

```bash
workbuddy issue restart --repo OWNER/NAME --issue 173
```

- 删除该 issue 的 `issue_cache` 行，让下一次 poll 把它当成新 issue
- 清除 `issue_dependency_state`，避免沿用旧 verdict
- 如果存在残留 `issue_claim`，一并删除
- 清除 dev↔review cycle state，避免 cycle-cap 旧记录继续卡住后续派发
- 如果能连到运行中的 coordinator（显式 `--coordinator`，或从 deploy manifest 自动发现），会额外调用 `POST /api/v1/admin/issues/{owner}/{repo}/{issue}/clear-inflight` 清掉进程内 inflight map
- 如果 coordinator 当时不可达，下一次 dispatch 也会基于 `task_queue` 自愈：当 backing task 行已删除、已变成 `failed`/`timeout`，或 lease 过期超过 `5×lease_duration` 时，会自动丢弃泄漏的 inflight 记录并继续派发
- 记录 `issue_restarted` 事件，方便事后审计
- `workbuddy admin restart-issue ...` 仍可用，但会在 stderr 打印 deprecation warning

相比直接操作 SQLite，这个命令把手工恢复流程固化成了可审计的 CLI。

`dispatch_skipped_inflight` 事件现在带 `task_status` 字段，operator 直接看事件流就能区分“确实还在跑”（例如 `pending` / `running`）还是“内存 inflight 泄漏”（`gone` / `failed` / `timeout`）。

### diagnose（REQ-037）

自动诊断 pipeline 常见故障模式：

```bash
workbuddy diagnose [--repo R] [--fix] [--json]
```

检测项：

| 检测 | 条件 | 严重度 |
| --- | --- | --- |
| Stuck issue | 中间状态 + 无 active task + 最后事件 > 1h | error |
| Missed redispatch | verdict=ready + 无 active task + 中间状态 | warn |
| Orphaned task | running + updated_at > 2x agent timeout | error |
| Repeated failure | 同 issue+agent 连续 failed >= 3 | warn |

`--fix` 对 stuck 和 missed redispatch 自动执行 cache-invalidate；repeated failure 只报告不修复。

## 互斥约束

`--tasks`、`--events`、`--watch`、`--stuck` 四个 flag 互斥，同时指定返回明确错误。

`status --stuck` 除了传统的“中间状态 + 最后事件超过 1h”外，也会把最后一个事件是
`dispatch_skipped_claim` 的 issue 直接视为 stuck，这样 coordinator 被旧 claim 卡住时会马上出现在 operator 视图里。

## 主要代码

- `cmd/status.go` — status 命令及 --tasks/--events/--watch 子模式
- `cmd/cache_invalidate.go` — `cache invalidate` / `cache-invalidate` 命令
- `cmd/admin_restart_issue.go` — `issue restart` / `admin restart-issue` 命令
- `cmd/diagnose.go` — diagnose 命令
- `internal/operator/detector.go` — 进程内自愈告警 detector
- `internal/diagnose/diagnose.go` — 诊断逻辑（纯逻辑，无副作用）
- `internal/tasknotify/hub.go` — SSE Pub/Sub hub
- `internal/audit/http.go` — /tasks、/events HTTP 端点
- `internal/auditapi/handler.go` — `/api/v1/alerts` dashboard API
