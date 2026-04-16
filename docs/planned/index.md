# Planned Docs

这里记录明确是目标态、但还没有完全落到当前代码里的设计。

## 设计主题

| 主题 | 目标 | 基线代码 | 依赖前置 |
| --- | --- | --- | --- |
| Long-lived runtime / pooling | 为长驻 runtime 增加 per-repo 生命周期、连接复用与 idle 回收 | `internal/launcher/`, `cmd/serve.go` | 现有 one-shot Session 抽象已完成 |

## 文档列表

（当前无活跃的 planned 文档——原有的 distributed-topology-and-cli、runtime-migration-plan、pipeline-observability-and-diagnosis 均已落地，迁移至 `docs/implemented/`。）

## 维护规则

- 这里写的是目标，不是现状。
- 每份文档都必须说明和当前代码之间的距离。
- 任何 planned 文档落地之后，都要迁移到 `docs/implemented/`。
