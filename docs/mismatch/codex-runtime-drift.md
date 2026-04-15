# Codex Runtime Drift

状态：mismatch

## 现象

`.github/workbuddy/agents/codex-dev-agent.md` 已经把 Codex 描述成一个更结构化的 runtime：

- 使用 `codex exec --json`
- 通过 `--output-last-message` 产出更干净的最终消息
- 暗示 reporter / audit 可以更好地消费这些结构化产物

但当前执行链路还没有真正把这些能力升级成统一 runtime 语义。

## 当前代码事实

当前 Codex runtime 仍然属于 one-shot `Launch(...)`：

- 执行 `command`
- 把 stdout/stderr 作为整体缓冲读取
- 返回 `launcher.Result`
- reporter 仍按最终结果格式化 issue comment
- audit 会做摘要，但不是 Event v1 级别结构化消费

代码：

- `internal/launcher/codex.go`
- `internal/launcher/process.go`
- `internal/reporter/reporter.go`
- `internal/audit/audit.go`

## 差异点

### 1. `--json` 已经写进 agent 文档，但平台层还没有统一解析协议

目前只是“命令本身可能输出 JSONL”，不是“系统已经消费 JSONL event stream”。

### 2. `--output-last-message` 已经写进命令，但 reporter 没有把它当成统一主字段

这类信息还没有提升成 planned 文档里的 `Result.LastMessage`。

### 3. Codex app-server 仍然完全是规划能力

当前仓库里没有 app-server runtime，也没有 per-repo session pool。

## 建议收敛方式

- 如果短期不做统一结构化消费，就把 codex agent 文档表述收窄成“当前只是运行参数建议”。
- 如果要兑现这条路线，就按 `docs/planned/runtime-session-architecture.md` 和 `docs/planned/event-schema-v1.md` 继续实现。

## 依赖关系

- 兑现路线 blocked by：`docs/planned/event-schema-v1.md`、`docs/planned/runtime-session-architecture.md`
- 这条 drift 与 planned 中的 Codex app-server 设计同源；planned 文档落地后此 drift 自动消除。
