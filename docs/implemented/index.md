# Implemented Docs

这里记录已经能够直接代表当前代码行为的文档。

## 范围

| 主题 | 结论 | 主要代码 |
| --- | --- | --- |
| 运行形态 | 当前只有 `workbuddy serve` 单进程模式可用 | `cmd/serve.go` |
| 执行模型 | Router 通过 channel 派发给内嵌 worker，runtime 已升级为 `Start(...) -> Session.Run(...)`，并保留 `Launch(...)` 兼容包装 | `internal/router/router.go`, `internal/launcher/types.go`, `internal/launcher/process.go` |
| 配置模型 | 使用 `.github/workbuddy/` 下的 Markdown + YAML frontmatter | `internal/config/loader.go`, `internal/config/types.go` |
| 工作流模型 | 以 issue label 为主驱动；Go 侧记录 retry/failure intent，agent 通过 `gh issue edit` 推进状态 | `internal/statemachine/statemachine.go`, `internal/store/store.go`, `.github/workbuddy/workflows/*.md` |
| 可观测性 | 已有 SQLite、event log、Event Schema v1 session artifact、session audit、`/sessions` Web UI | `internal/store/`, `internal/eventlog/`, `internal/launcher/`, `internal/audit/`, `internal/webui/` |
| 工作区隔离 | 已支持每任务 git worktree 隔离 | `internal/workspace/workspace.go` |

## 文档列表

| 文档 | 说明 | 主要代码 |
| --- | --- | --- |
| `docs/implemented/current-architecture.md` | 当前系统形态、主链路、GH call boundary 和 retry/failure 事实 | `cmd/serve.go`, `internal/router/router.go`, `internal/statemachine/statemachine.go`, `internal/store/store.go` |
| `docs/implemented/current-config-workflow-and-agents.md` | 当前 config/agent/workflow schema 与触发行为 | `internal/config/`, `.github/workbuddy/agents/`, `.github/workbuddy/workflows/` |
| `docs/implemented/agent-schema-vnext.md` | agent schema vNext 的兼容边界、policy/prompt/output_contract 与校验行为 | `internal/config/`, `internal/launcher/`, `.github/workbuddy/agents/` |
| `docs/implemented/agent-catalog.md` | 仓库样例当前已登记的 agent catalog 与各自 schema/output contract | `.github/workbuddy/agents/`, `internal/config/loader.go` |
| `docs/implemented/current-runtime-reporting-and-audit.md` | 当前 launcher、reporter、audit、sessions UI 行为 | `internal/launcher/`, `internal/reporter/`, `internal/audit/`, `internal/webui/` |
| `docs/implemented/event-schema-v1.md` | 当前 Event Schema v1 合同、runtime 映射、artifact 消费路径 | `internal/launcher/events/`, `internal/launcher/codex.go`, `internal/launcher/claude_stream.go`, `cmd/serve.go`, `internal/audit/`, `internal/webui/` |
| `docs/implemented/current-persistence-and-workspace.md` | 当前存储、事件日志、worker registry、worktree 隔离 | `internal/store/`, `internal/eventlog/`, `internal/registry/`, `internal/workspace/` |
| `docs/implemented/artifact-layout.md` | 当前 session artifact 位于仓库根 `.workbuddy/sessions/`，不会随 worktree 清理丢失 | `cmd/serve.go`, `cmd/run.go`, `internal/launcher/`, `internal/router/router.go`, `internal/audit/` |

## 维护规则

- 这里只保留当前代码已经兑现的事实。
- 一旦发现行为和代码脱节，先把内容移到 `docs/mismatch/`，不要继续在这里堆设计设想。
