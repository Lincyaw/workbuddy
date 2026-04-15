# Retry and Failure Drift

状态：mismatch

## 现象

workflow 文档和旧 roadmap 会让人以为：

- `max_retries` 已经是非常稳定的行为
- 超限后一定自动切到 `failed`
- `failed` 后一定自动加 `needs-human`

但当前代码并没有把这条链路实现成“可以无条件信赖的已完成能力”。

## 当前代码确实有的部分

当前 state machine 已经会：

- 记录 transition count
- 识别 back-edge
- 在超过 `max_retries` 时记录 `TypeCycleLimitReached` 和 `TypeTransitionToFailed`

代码：

- `internal/statemachine/statemachine.go`
- `internal/store/store.go`

## 当前还缺的关键闭环

### 1. Go 侧不会真正替 agent 写入失败标签

注释里已经写明：Go 侧记录 intent，但不负责实际 label 写操作。

### 2. `failed` / `needs-human` 更像记录语义，不是完整执行闭环

旧文档把这件事写成“系统自动完成”，但当前更准确的说法是：

- 系统能检测到应该失败
- 系统会记录这个判断
- 但 label 写回与后续人工介入机制并没有全部闭环化

### 3. 去重与轮询时序仍会影响 retry 语义稳定性

当前还有：

- poll 周期级别的 dedup
- label 变化和 task 完成之间的竞态
- 依赖 agent 自己路由的外部性

这些都意味着 retry 行为还不适合在 implemented 文档里表述成“完全可靠自动化”。

## 建议收敛方式

二选一：

1. 扩代码：把 failed label / needs-human 的写回和后续动作做成完整系统能力。
2. 收文档：明确 retry 目前只是“检测 + 记录 + 部分约束”，不是最终态自动失败编排。
