# Event Schema v1

状态：planned

## 目标

Event Schema v1 是未来 runtime、audit、reporter、web UI 的统一语言层。

它的价值在于：

- 事件实时记录，而不是等进程退出再补摘要
- 不同 runtime 只需要做 native event -> Event v1 映射
- reporter、audit、UI 不再分别解析 Claude/Codex 私有协议

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

冻结原则：

- `kind` 在 v1 内不改名、不删除
- `payload` 只做向后兼容扩展
- `raw` 始终保留原始 runtime 事件，避免信息丢失

## 冻结事件种类与 Payload

v1 冻结 12 个 EventKind。每个 kind 对应一个固定 payload 结构。冻结意味着：

- `kind` 名称不删、不改、不重命名
- payload **不删字段、不改字段类型**，只允许新增可选字段
- 任何破坏性变更必须发布为 v2

### EventKind 与 Payload 字段表

| Kind | Payload 字段 | 说明 |
| --- | --- | --- |
| `turn.started` | `turn_id string` | 一次 turn 的开始；oneshot runtime 可在进程启动时发一次 |
| `turn.completed` | `turn_id string`, `status "ok"\|"error"\|"interrupted"` | turn 终态 |
| `agent.message` | `text string`, `delta bool`, `final bool` | 模型正文。`delta=true` 表示流式增量；`final=true` 表示该消息已完整 |
| `reasoning` | `text string`, `delta bool` | 推理内容。可能不出现（runtime 不产出时） |
| `tool.call` | `name string`, `call_id string`, `args json.RawMessage` | 模型发起工具调用；`call_id` 用来串联 result |
| `tool.result` | `call_id string`, `ok bool`, `result json.RawMessage` | 工具调用返回；与 `tool.call` 同一 `call_id` |
| `command.exec` | `cmd []string`, `cwd string`, `call_id string` | 子进程命令开始（shell / git / gh） |
| `command.output` | `call_id string`, `stream "stdout"\|"stderr"`, `data string` | 子进程输出增量 |
| `file.change` | `path string`, `change_kind "create"\|"modify"\|"delete"`, `diff string` | 文件变更增量；`diff` 为 unified diff |
| `token.usage` | `input int`, `output int`, `cached int`, `total int` | 累计或单 turn 的 token 使用量 |
| `error` | `code string`, `message string`, `recoverable bool` | runtime 或会话级错误 |
| `log` | `stream "stdout"\|"stderr"`, `line string` | 兜底：未结构化的原始输出行 |

### Payload 结构体放置位置

- 路径：`internal/launcher/events/`
- 每个 kind 一个文件：`agent_message.go`、`command_exec.go` ...
- 每个文件导出两类符号：`type AgentMessagePayload struct{...}` 和常量 `KindAgentMessage EventKind = "agent.message"`

## 推荐落地方式

- event 定义建议放在 `internal/launcher/events/`
- one-shot runtime 至少要能产出 `log`、`turn.completed`、必要的 `error`
- Claude stream-json、Codex JSONL 优先做结构化映射
- 审计链路保持双轨：实时查询看 Event v1，事后取证看 session artifact

## Runtime 映射表

下面三张表是**实现合同**：runtime 实现者必须按表把 native event 映射到 EventKind。表里没列的 native event 走 `log` 兜底（带 `raw` 字段保留原始数据），不允许私自新增 EventKind。

### Codex `exec --json` → Event v1

JSONL 每行一个 codex 事件，`msg.type` 决定映射。

| codex JSONL `msg.type` | EventKind | 字段映射 |
| --- | --- | --- |
| `task_started` | `turn.started` | `turn_id` ← `msg.task_id`（或自生 UUID） |
| `agent_message` / `agent_message_delta` | `agent.message` | `text` ← `msg.message`；`delta` ← 是否 delta 类型；`final` ← 非 delta |
| `agent_reasoning` / `agent_reasoning_delta` | `reasoning` | `text` ← `msg.text`；`delta` 同上 |
| `exec_command_begin` | `command.exec` | `cmd` ← `msg.command`；`cwd` ← `msg.cwd`；`call_id` ← `msg.call_id` |
| `exec_command_output_delta` | `command.output` | `call_id` ← `msg.call_id`；`stream` ← `msg.stream`；`data` ← `msg.chunk` |
| `exec_command_end` | `tool.result` | `call_id` ← `msg.call_id`；`ok` ← `msg.exit_code == 0`；`result` ← 完整 msg |
| `mcp_tool_call_begin` | `tool.call` | `name` ← `msg.invocation.tool`；`call_id` ← `msg.call_id`；`args` ← `msg.invocation.arguments` |
| `mcp_tool_call_end` | `tool.result` | `call_id` ← `msg.call_id`；`ok` ← `msg.is_error == false`；`result` ← `msg.result` |
| `patch_apply_begin` | `file.change` | `path` 多文件时拆多条；`change_kind` 来自 patch；`diff` ← patch 内容 |
| `token_count` | `token.usage` | `input` ← `msg.input_tokens`；`output` ← `msg.output_tokens`；`cached` ← `msg.cached_input_tokens`；`total` ← input+output |
| `task_complete` | `turn.completed` | `status: "ok"` |
| `error` | `error` | `code` ← `msg.code`（无则 `"unknown"`）；`message` ← `msg.message`；`recoverable: false` |
| 其它 | `log` | `stream: "stdout"`；`line` ← 原始 JSON 行；`raw` 保留 |

### Codex `app-server` JSON-RPC notification → Event v1

| RPC method | EventKind | 字段映射 |
| --- | --- | --- |
| `thread/started` | (不映射，存进 `SessionRef.ID`) | — |
| `turn/started` | `turn.started` | `turn_id` ← `params.turn.id` |
| `turn/completed` | `turn.completed` | `turn_id` ← `params.turn.id`；`status: "ok"` |
| `item/agentMessage/delta` | `agent.message` | `text` ← `params.delta`；`delta: true`；`final: false` |
| `item/completed`（kind=agent_message） | `agent.message` | `text` ← `params.item.text`；`delta: false`；`final: true` |
| `item/reasoning/textDelta` | `reasoning` | `text` ← `params.delta`；`delta: true` |
| `item/reasoning/summaryTextDelta` | `reasoning` | 同上 |
| `item/started`（kind=command_execution） | `command.exec` | `cmd` ← `params.item.command`；`cwd` ← `params.item.cwd`；`call_id` ← `params.item.id` |
| `item/commandExecution/outputDelta` / `command/exec/outputDelta` | `command.output` | `call_id` ← `params.callId`；`stream` / `data` ← params |
| `item/completed`（kind=command_execution） | `tool.result` | `call_id` ← `params.item.id`；`ok` ← `params.item.exit_code == 0` |
| `item/started`（kind=mcp_tool_call） | `tool.call` | `name` / `call_id` / `args` ← params |
| `item/mcpToolCall/progress` | `tool.result` (partial) | 进度信息走 result，`ok` 暂记 `true` |
| `turn/diff/updated` | `file.change` | 多文件拆多条；`path` / `change_kind` / `diff` ← params.diff |
| `item/fileChange/outputDelta` | `file.change` | 同上，增量 |
| `thread/tokenUsage/updated` | `token.usage` | `input` / `output` / `cached` / `total` ← params |
| `error` | `error` | `code` / `message` ← params；`recoverable` ← 视错误类型 |
| `deprecationNotice` / `configWarning` | `log` | `line` ← 序列化后内容 |
| `model/rerouted` | `log` | `line` ← 序列化（v2 可考虑提升为独立 kind） |
| 其它（包括 thread/* 元数据通知） | `log` | `raw` 保留 |

server→client **request** 类（如 `execCommandApproval` / `applyPatchApproval`）不映射为 Event，而是走 `Approver` 接口；但调用前后会发 `tool.call` / `tool.result` 用于 audit 留痕。

### Claude `--output-format stream-json` → Event v1

| Claude stream message | EventKind | 字段映射 |
| --- | --- | --- |
| `system.init` | `turn.started` | `turn_id` ← `session_id` |
| `assistant.message_start` | (不映射，缓存 message_id) | — |
| `assistant.content_block_delta`（type=text_delta） | `agent.message` | `text` ← `delta.text`；`delta: true`；`final: false` |
| `assistant.content_block_delta`（type=thinking_delta） | `reasoning` | `text` ← `delta.thinking`；`delta: true` |
| `assistant.content_block_start`（type=tool_use） | `tool.call` | `name` ← `tool_use.name`；`call_id` ← `tool_use.id`；`args` ← `tool_use.input`（流式拼装） |
| `user.tool_result` | `tool.result` | `call_id` ← `tool_use_id`；`ok` ← `is_error == false`；`result` ← `content` |
| `assistant.message_stop` | `turn.completed` + `token.usage` | `status: "ok"`；usage 从 `usage` 字段提取 |
| `system.error` | `error` | `code` / `message` ← payload；`recoverable: false` |
| Bash 工具的 `tool.call` | 同时发 `command.exec` | `cmd` 从 `tool_use.input.command` 解析 |
| Bash 工具的输出 | `command.output` | 通过 `tool.result` 拆分（claude 不流式推 stdout，按结果一次发） |
| Edit/Write 工具结果 | `file.change` | `path` / `change_kind` / `diff` 从 input 解析 |
| 其它 | `log` | `raw` 保留 |

注：claude stream 不像 codex 那样实时推 shell 子进程 stdout，`command.output` 在 claude runtime 下只能在 `tool.result` 时一次性产出。这是协议限制，非实现缺陷。
