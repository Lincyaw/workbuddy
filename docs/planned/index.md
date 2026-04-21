# Planned Docs

这里记录明确是目标态、但还没有完全落到当前代码里的设计。

## 设计主题

| 主题 | 目标 | 基线代码 | 依赖前置 |
| --- | --- | --- | --- |
| Long-lived runtime / pooling | 为长驻 runtime 增加 per-repo 生命周期、连接复用与 idle 回收 | `internal/launcher/`, `cmd/serve.go` | 现有 one-shot Session 抽象已完成 |
| Worker execution boundary | 把 embedded / distributed worker 统一到共享执行核心，并抽出 `internal/worker/` / `internal/runtime/` 边界 | `cmd/serve.go`, `cmd/worker.go`, `internal/worker/`, `internal/runtime/`, `internal/ghadapter/`, `internal/launcher/` | shared executor + embedded/distributed extraction + GitHub/session boundary收口已落地；executor 已接管 worktree setup/cleanup，session storage degraded health 已显式落盘并写入 result metadata；`internal/agent/bridge.go` 删除、MustPayload 热路径移除已完成；launcher compatibility shim 彻底收尾仍待后续 slice |

## 文档列表

- `docs/planned/worker-execution-boundary.md` — `#146` 的 repo 内设计锚点，定义 worker 执行核心、runtime 收口与分阶段迁移切片。

## 维护规则

- 这里写的是目标，不是现状。
- 每份文档都必须说明和当前代码之间的距离。
- 任何 planned 文档落地之后，都要迁移到 `docs/implemented/`。
