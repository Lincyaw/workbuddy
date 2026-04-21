# Event Schema v1

状态：implemented

## 目标

Event Schema v1 现在是 runtime、audit、sessions Web UI 之间的统一事件语言层。

当前代码已经兑现的事实：

- `internal/launcher/events/` 冻结了 Event v1 顶层结构和 12 个 `EventKind`
- Codex `exec --json` 会实时映射成 Event v1
- Claude prompt 模式会消费 `stream-json` 并映射成 Event v1
- `internal/worker/executor.go` 的共享 session writer 会把统一事件流写入 `.workbuddy/sessions/<session>/events-v1.jsonl`
- audit 和 Web UI 优先消费这份统一 artifact

## 顶层结构

```go
type Event struct {
    Kind      EventKind       `json:"kind"`
    Timestamp time.Time       `json:"ts"`
    SessionID string          `json:"session_id"`
    TurnID    string          `json:"turn_id,omitempty"`
    Seq       uint64          `json:"seq"`
    Payload   json.RawMessage `json:"payload"`
    Raw       json.RawMessage `json:"raw,omitempty"`
}
```

代码：

- `internal/launcher/events/event.go`

## 冻结事件种类

当前实现冻结以下 12 个 `EventKind`：

- `turn.started`
- `turn.completed`
- `agent.message`
- `reasoning`
- `tool.call`
- `tool.result`
- `command.exec`
- `command.output`
- `file.change`
- `token.usage`
- `error`
- `log`

每个 kind 的 payload 结构都在 `internal/launcher/events/` 下按文件拆开定义。

## 当前 runtime 映射

### Codex app-server JSON-RPC

Codex runtime 会消费 `codex app-server --listen stdio://` 的 JSON-RPC 通知，并映射成统一事件：

- `turn/started` -> `turn.started`
- `item/agentMessage/delta` / `item/completed(agentMessage)` -> `agent.message`
- `item/reasoning/*` / `item/completed(reasoning)` -> `reasoning`
- `item/started(commandExecution)` -> `command.exec`
- `item/commandExecution/outputDelta` / `item/completed(commandExecution)` -> `command.output`
- `item/completed(commandExecution|mcpToolCall|dynamicToolCall)` -> `tool.result`
- `item/started(mcpToolCall|dynamicToolCall)` -> `tool.call`
- `item/completed(fileChange)` -> `file.change`
- `thread/tokenUsage/updated` -> `token.usage`
- `turn/completed` -> `turn.completed` + `task.complete`
- `error` -> `error`

代码：

- `internal/agent/codex/events.go`
- `internal/agent/codex/events_test.go`

### Claude prompt `stream-json`

当 runtime 实际启动 `claude` prompt 流时，launcher 会切到 `stream-json` 解析路径：

- `system.init` -> `turn.started`
- `assistant.content_block_delta` (`text_delta`) -> `agent.message`
- `assistant.content_block_delta` (`thinking_delta`) -> `reasoning`
- `assistant.content_block_start` (`tool_use`) -> `tool.call`
- Bash 工具调用同时补发 `command.exec`
- `user.tool_result` -> `tool.result`
- Bash 工具结果会补发 `command.output`
- Write/Edit 类工具结果会补发 `file.change`
- `assistant.message_stop` -> `token.usage` + `turn.completed`
- `system.error` -> `error`
- 其它未结构化通知 -> `log`

代码：

- `internal/launcher/claude_stream.go`
- `internal/launcher/claude_stream_test.go`
- `internal/launcher/process.go`

### 泛化 shell command 路径

`claude-code` runtime 仍然兼容非 Claude 原生命令的 shell one-shot 执行，例如测试里的 `echo "hello"`。这条兼容路径不会产生 Claude 原生结构化事件，只会产出：

- `turn.started`
- `log`（stderr）
- `error`
- `turn.completed`

这是为了保留现有 launcher/测试的兼容性，不属于 Claude 原生协议映射合同。

## Audit / Web UI / Reporter

### Audit

- `internal/worker/executor.go` 把 runtime 事件流落盘为 `events-v1.jsonl`
- `internal/audit/audit.go` 在读到 Event v1 artifact 时按 event count、command、token usage 生成摘要
- 若没有统一 artifact，audit 才退化为旧的 session/native 文件摘要

### Sessions Web UI

- `/sessions/<id>` 会探测是否存在 `events-v1.jsonl`
- `/sessions/<id>/events.json` 直接返回 Event v1 的裁剪视图
- `/sessions/<id>/stream` 用 SSE tail 同一份事件文件

### Reporter

- reporter 仍然以 `launcher.Result` 为 issue comment 输入
- 但 `Result` 里的 `LastMessage`、`TokenUsage`、`SessionPath` 已经和统一 session/event 流对齐
- 在 `serve` 主链路里，`Result.SessionPath` 会优先指向归一化后的 `events-v1.jsonl`

## 相关代码

- `internal/launcher/events/`
- `internal/launcher/agent_bridge.go`
- `internal/agent/codex/events.go`
- `internal/launcher/claude_stream.go`
- `internal/launcher/process.go`
- `internal/worker/executor.go`
- `internal/audit/audit.go`
- `internal/webui/handler.go`
- `internal/reporter/reporter.go`
