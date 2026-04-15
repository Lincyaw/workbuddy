# Runtime Migration Plan

状态：planned

## v0.1.x 迁移目标

优先做低风险抽象重构，而不是一次性引入所有未来协议：

1. 把 `Runtime.Launch(...)` 升级为 `Runtime.Start(...) -> Session`。
2. 让现有 Claude/Codex 先变成 one-shot Session 适配器。
3. 落地 Event Schema v1 的最小闭环。
4. 在 agent schema 中增加 `policy`、`prompt`、`output_contract`，但不移除 `command`。
5. 先提供最小 Approver（例如 `AlwaysAllow`）。

## command 字段 deprecation 时间窗

为防止"forever 兼容"成包袱，固定节奏：

| 版本 | `command` 状态 |
| --- | --- |
| v0.1.x | 与 `prompt` + `policy` 并存。runtime 优先读 `prompt`，无则读 `command` |
| v0.2.0 | 加载阶段对 `command` 报 deprecation 警告。所有内置 agent md 必须迁移完毕 |
| v0.3.0 | 移除 `command` 字段，loader 报错 |

## v0.2.x 以后

后续再逐步引入：

- `codex-appserver`
- per-repo app-server 连接池
- 动态审批回调
- 分布式 worker/coordinator 通信
- 更强的实时 UI 与事件回放

## 迁移顺序约束

建议顺序：

1. 先统一 Runtime/Session 生命周期。
2. 再统一 Event Schema。
3. 再改 audit/reporter/web UI 消费链路。
4. 最后再接入长驻 runtime 和 distributed transport。

这样可以避免同时重写执行层、协议层和运维层。

## Label 校验的迁移原则

未来即使引入 Session 和 Event，也不建议马上让 Go 侧接管 label 变更：

- 继续记录执行前 label 集合
- Session 结束后回读 label
- 校验是否发生合法迁移
- 发现异常时记录 audit/report，而不是自动代改标签

这样能保持当前 Agent-as-Router 语义连续性。

具体的校验规则表（合法转移 / 无变化 / 非法转移 / failed）见 `docs/planned/runtime-session-architecture.md` 的 "Agent 退出后的 label 校验" 一节。
