# Mismatch Docs

这里记录“当前代码”和“现有需求表达”之间仍然不一致的部分。

## 差异清单

| 文档 | 差异主题 | 涉及代码 |
| --- | --- | --- |
| `docs/mismatch/config-schema-drift.md` | `config.yaml` 展示字段和真实 loader 支持字段不一致 | `.github/workbuddy/config.yaml`, `internal/config/types.go` |
| `docs/mismatch/poller-and-state-machine-drift.md` | Poller、StateMachine 的职责边界和旧设计不一致 | `internal/poller/poller.go`, `internal/statemachine/statemachine.go` |
| `docs/mismatch/retry-and-failure-drift.md` | `max_retries`、failed 状态和自动回退能力没有旧文档说得那么完整 | `internal/statemachine/statemachine.go`, `internal/store/store.go` |
| `docs/mismatch/codex-runtime-drift.md` | Codex agent 文档对结构化输出消费的表述超前于当前 launcher/reporter | `.github/workbuddy/agents/codex-dev-agent.md`, `internal/launcher/codex.go` |
| `docs/mismatch/roadmap-structure-drift.md` | 旧 roadmap 中的模块命名、命令清单、分阶段产出与当前代码布局不一致 | `cmd/`, `internal/` |

## 使用规则

- 这类差异在收敛前，不能直接拿来指导代码修改。
- 先决定要“改代码补齐”，还是“收窄文档回到事实”。
- 收敛完成后，把文档迁回 `implemented` 或 `planned`。
