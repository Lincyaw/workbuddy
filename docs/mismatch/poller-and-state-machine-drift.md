# Poller and State Machine Drift

状态：mismatch

## 旧设计与现状的核心偏差

旧设计里对 Poller 和 StateMachine 有更强的能力期待，但当前代码更收敛、更被动。

## Poller 实际能力

当前 Poller 只负责：

- 拉 open issues
- 拉 open PRs
- 做 cache diff
- 发出少量 change events

当前事件集合只有：

- `issue_created`
- `label_added`
- `label_removed`
- `pr_created`
- `pr_state_changed`
- `issue_closed`

代码：

- `internal/poller/poller.go`

旧设计中提到但当前未实现的方向包括：

- PR review state
- checks rollup
- comment command
- 更丰富的 PR 驱动 transition

## StateMachine 实际职责

当前 StateMachine 更像“标签变更观察器 + dispatch 触发器”：

- 匹配 workflow
- 从 labels 推断当前状态
- 在状态进入时 dispatch agent
- 记录 transition count、event log、stuck 相关信息

它并不会统一替 agent 执行 GitHub label 写操作。

代码：

- `internal/statemachine/statemachine.go`

## 当前真实协作方式

当前真相是：

- agent 进程自己通过 `gh issue edit` 推动状态迁移
- Go 侧只根据后续 poll 看到的 label 变化做响应
- 这是一种“Go 侧被动观察 + agent 主动路由”的模式

如果未来想把状态迁移收回 Go 侧，就不应该直接在旧 broad doc 上继续加描述，而要先更新 planned 文档并明确迁移原则。
