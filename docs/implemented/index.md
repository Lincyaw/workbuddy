# Implemented Docs

这里记录已经能够直接代表当前代码行为的文档。

## 范围

| 主题 | 结论 | 主要代码 |
| --- | --- | --- |
| 运行形态 | `workbuddy serve`（单机）和 `workbuddy coordinator` + `workbuddy worker`（分布式）两种模式 | `cmd/serve.go`, `cmd/coordinator.go`, `cmd/worker.go` |
| 执行模型 | Router 通过 channel（v0.1.0）或 HTTP 长轮询（v0.2.0）派发给 Worker，canonical runtime 已升级为 `Start(...) -> Session.Run(...)`，并保留 `Launch(...)` 兼容包装 | `internal/router/router.go`, `internal/runtime/`, `internal/launcher/process.go` |
| 配置模型 | 使用 `.github/workbuddy/` 下的 Markdown + YAML frontmatter | `internal/config/loader.go`, `internal/config/types.go` |
| 工作流模型 | 以 issue label 为主驱动；Go 侧记录 retry/failure intent，agent 通过 `gh issue edit` 推进状态 | `internal/statemachine/statemachine.go`, `internal/store/store.go`, `.github/workbuddy/workflows/*.md` |
| Issue 依赖 | 已支持 `workbuddy.depends_on` 解析、本地 verdict/queue、dispatch hard gate、以及 😕 反应信号 | `cmd/serve.go`, `internal/dependency/`, `internal/store/`, `internal/statemachine/`, `internal/router/` |
| 可观测性 | SQLite、event log、Event Schema v1 session artifact、session audit、`/sessions` Web UI、CLI 诊断工具 | `internal/store/`, `internal/eventlog/`, `internal/launcher/`, `internal/audit/`, `internal/webui/`, `cmd/status.go`, `cmd/diagnose.go` |
| 工作区隔离 | 已支持每任务 git worktree 隔离 | `internal/workspace/workspace.go` |
| 分布式通信 | Coordinator HTTP API + Worker 长轮询 + 共享密钥认证 | `internal/coordinator/http/`, `internal/workerclient/` |

## 文档列表

| 文档 | 说明 | 主要代码 |
| --- | --- | --- |
| `current-architecture.md` | 当前系统形态、主链路、GH call boundary 和 retry/failure 事实 | `cmd/serve.go`, `internal/router/`, `internal/statemachine/`, `internal/store/` |
| `current-config-workflow-and-agents.md` | 当前 config/agent/workflow schema 与触发行为 | `internal/config/`, `.github/workbuddy/agents/`, `.github/workbuddy/workflows/` |
| `agent-schema-vnext.md` | agent schema vNext 的兼容边界、policy/prompt/output_contract 与校验行为 | `internal/config/`, `internal/launcher/`, `.github/workbuddy/agents/` |
| `agent-catalog.md` | 2-agent catalog（dev-agent, review-agent）与各自 schema/output contract | `.github/workbuddy/agents/`, `internal/config/loader.go` |
| `current-runtime-reporting-and-audit.md` | launcher、reporter、audit、sessions UI 行为 | `internal/launcher/`, `internal/agent/codex/`, `internal/reporter/`, `internal/audit/`, `internal/webui/` |
| `remote-runner-github-actions.md` | GitHub Actions remote runner 的 agent config、dispatch/poll 行为 | `internal/config/`, `internal/launcher/`, `.github/workflows/` |
| `runtime-session-architecture.md` | Runtime.Start → Session.Run 主链路，post-Run label validation | `internal/worker/embedded.go`, `internal/worker/executor.go`, `internal/runtime/`, `internal/launcher/`, `internal/labelcheck/`, `internal/reporter/`, `internal/audit/` |
| `event-schema-v1.md` | Event Schema v1 合同、runtime 映射、artifact 消费路径 | `internal/launcher/events/`, `internal/launcher/agent_bridge.go`, `internal/agent/codex/events.go`, `internal/launcher/claude_stream.go` |
| `current-persistence-and-workspace.md` | 存储、事件日志、worker registry、worktree 隔离 | `internal/store/`, `internal/eventlog/`, `internal/registry/`, `internal/workspace/` |
| `artifact-layout.md` | session artifact 位于仓库根 `.workbuddy/sessions/` | `cmd/serve.go`, `cmd/run.go`, `internal/worker/executor.go`, `internal/launcher/`, `internal/audit/` |
| `issue-dependencies.md` | issue dependency 声明、verdict、dispatch gate、😕 反应信号 | `cmd/serve.go`, `internal/dependency/`, `internal/store/`, `internal/statemachine/` |
| `audit-http-server.md` | REQ-011 审计 HTTP 端点：/events、/issues/.../state、/sessions/:id | `cmd/serve.go`, `internal/auditapi/`, `internal/webui/` |
| `distributed-topology-and-cli.md` | Coordinator/Worker 分布式拓扑、全部 CLI 命令列表、HTTP API | `cmd/coordinator.go`, `cmd/worker.go`, `internal/coordinator/http/` |
| `runtime-migration-plan.md` | Runtime/Session 迁移完成状态、command deprecation 路线 | `internal/runtime/`, `internal/launcher/process.go` |
| `pipeline-observability-and-diagnosis.md` | status --tasks/--events/--watch、cache-invalidate、diagnose | `cmd/status.go`, `cmd/diagnose.go`, `cmd/cache_invalidate.go` |

## 维护规则

- 这里只保留当前代码已经兑现的事实。
- 一旦发现行为和代码脱节，先把内容移到 `docs/mismatch/`，不要继续在这里堆设计设想。
