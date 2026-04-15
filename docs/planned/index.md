# Planned Docs

这里记录明确是目标态、但还没有完全落到当前代码里的设计。

## 设计主题

| 主题 | 目标 | 基线代码 | 依赖前置 |
| --- | --- | --- | --- |
| Runtime / Session 抽象 | 从 `Launch(...)` 升级为 `Runtime.Start(...) -> Session` | `internal/launcher/` | — |
| Event Schema v1 | 给 runtime、audit、web UI、reporter 一个统一事件模型 | `internal/launcher/`, `internal/audit/`, `internal/webui/` | Runtime / Session 抽象 |
| Agent schema vNext | 在保留 `command` 兼容性的前提下，引入 `policy`、`prompt`、`output_contract` | `internal/config/`, `.github/workbuddy/agents/` | Runtime / Session 抽象 |
| Agent catalog | 登记规划中的 agent 类型，作为批量开 issue 的索引 | `.github/workbuddy/agents/` | Agent schema vNext |
| 迁移路径 | 控制 v0.1.x 到 v0.2.x 的重构顺序 | `internal/launcher/`, `internal/config/`, `internal/audit/` | 上述全部 |
| 分布式拓扑与 CLI | 从单进程 `serve` 演进到 coordinator/worker 分离 | `cmd/`, `internal/router/`, `internal/registry/` | 独立，不依赖上述 |

## 文档列表

- `docs/planned/runtime-session-architecture.md`
- `docs/planned/agent-schema-vnext.md`
- `docs/planned/event-schema-v1.md`
- `docs/planned/agent-catalog.md`
- `docs/planned/distributed-topology-and-cli.md`
- `docs/planned/runtime-migration-plan.md`

## 维护规则

- 这里写的是目标，不是现状。
- 每份文档都必须说明和当前代码之间的距离。
- 任何 planned 文档落地之后，都要迁移到 `docs/implemented/`。
