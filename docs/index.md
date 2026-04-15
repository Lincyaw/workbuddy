# Workbuddy Documentation

当前文档只保留三类目录，所有需求讨论、代码修改和后续维护都以这三类文档为准：

- `docs/implemented/`: 已经和当前代码一致的事实文档。
- `docs/planned/`: 明确是目标态、尚未落地的设计文档。
- `docs/mismatch/`: 代码与需求表达仍不一致，需要先收敛再开发的文档。

## 使用规则

1. 先判断需求属于 `implemented`、`planned` 还是 `mismatch`。
2. 先改对应文档，再改代码。
3. 代码落地后，把文档移动到正确目录。
4. `project-index.yaml` 是文档与代码的唯一索引入口，新增或迁移文档时必须同步更新。

## 文档总览

| 目录 | 含义 | 适用场景 |
| --- | --- | --- |
| `docs/implemented/` | 当前真实行为 | review 现有实现、修 bug、增量扩展已上线能力 |
| `docs/planned/` | 目标设计 | 做新能力设计、拆阶段演进、定义未来 schema |
| `docs/mismatch/` | 差异清单 | 发现文档不能直接指导开发时，先在这里澄清 |

## 当前文档清单

### Implemented

- `docs/implemented/index.md`
- `docs/implemented/current-architecture.md`
- `docs/implemented/current-config-workflow-and-agents.md`
- `docs/implemented/current-runtime-reporting-and-audit.md`
- `docs/implemented/current-persistence-and-workspace.md`

### Planned

- `docs/planned/index.md`
- `docs/planned/runtime-session-architecture.md`
- `docs/planned/agent-schema-vnext.md`
- `docs/planned/event-schema-v1.md`
- `docs/planned/agent-catalog.md`
- `docs/planned/distributed-topology-and-cli.md`
- `docs/planned/runtime-migration-plan.md`

### Mismatch

- `docs/mismatch/index.md`
- `docs/mismatch/codex-runtime-drift.md`
