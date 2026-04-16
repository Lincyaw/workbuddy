# Pipeline Observability and Diagnosis

状态：implemented (v0.2.0)

## 概述

将原 `pipeline-monitor` skill 中的高频手动操作下沉为 CLI 一等公民，提供结构化的 pipeline 观测和自动诊断能力。对应 REQ-033 ~ REQ-037。

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

### cache-invalidate（REQ-034）

手动清除 issue 缓存，强制 poller 重新评估：

```bash
workbuddy cache-invalidate --repo OWNER/NAME --issue 47,48,49
```

- 删除 `issue_cache` 行
- 重置 `issue_dependency_state`（清除 verdict，强制重评估）
- 记录 `cache_invalidated` 事件
- 直连 SQLite，不依赖 serve 进程

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

## 主要代码

- `cmd/status.go` — status 命令及 --tasks/--events/--watch 子模式
- `cmd/cache_invalidate.go` — cache-invalidate 命令
- `cmd/diagnose.go` — diagnose 命令
- `internal/diagnose/diagnose.go` — 诊断逻辑（纯逻辑，无副作用）
- `internal/tasknotify/hub.go` — SSE Pub/Sub hub
- `internal/audit/http.go` — /tasks、/events HTTP 端点
