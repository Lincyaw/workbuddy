# Planned Docs

这里记录明确是目标态、但还没有完全落到当前代码里的设计。

## 设计主题

| 主题 | 目标 | 基线代码 | 依赖前置 |
| --- | --- | --- | --- |
| Long-lived runtime / pooling | 为长驻 runtime 增加 per-repo 生命周期、连接复用与 idle 回收 | `internal/launcher/`, `cmd/serve.go` | 现有 one-shot Session 抽象已完成 |
| 迁移路径 | 控制 v0.1.x 到 v0.2.x 的重构顺序 | `internal/launcher/`, `internal/config/`, `internal/audit/` | 上述全部 |
| 分布式拓扑与 CLI | 从单进程 `serve` 演进到 coordinator/worker 分离 | `cmd/`, `internal/router/`, `internal/registry/` | 独立，不依赖上述 |
| Pipeline 可观测性与诊断 | 将 pipeline-monitor skill 中高频手动操作下沉为 CLI 一等公民 | `cmd/status.go`, `internal/audit/http.go`, `internal/store/` | REQ-019 (status), REQ-024 (store) |
## 文档列表

- `docs/planned/distributed-topology-and-cli.md`
- `docs/planned/runtime-migration-plan.md`
- `docs/planned/pipeline-observability-and-diagnosis.md`

## 维护规则

- 这里写的是目标，不是现状。
- 每份文档都必须说明和当前代码之间的距离。
- 任何 planned 文档落地之后，都要迁移到 `docs/implemented/`。
