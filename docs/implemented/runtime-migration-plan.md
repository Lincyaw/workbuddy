# Runtime Migration — 完成状态

状态：implemented

## 已完成的迁移

以下迁移步骤在 v0.1.0~v0.2.0 期间全部落地：

### 1. Runtime/Session 生命周期统一

`Runtime.Launch(...)` 已升级为 `Runtime.Start(...) -> Session`，现有 Claude Code 和 Codex runtime 均实现了 Session 接口。`Launch()` 保留为兼容包装。

详见：`docs/implemented/runtime-session-architecture.md`

### 2. Event Schema v1

统一事件格式已落地。所有 runtime 产出的 session artifact 遵循 `events-v1.jsonl` 格式，downstream（audit、reporter、web UI）统一消费。

详见：`docs/implemented/event-schema-v1.md`

### 3. Agent Schema 扩展

Agent 定义新增 `policy`、`prompt`、`output_contract` 字段，与 `command` 并存。Launcher 支持 JSON Schema 校验 agent 输出。

详见：`docs/implemented/agent-schema-vnext.md`

### 4. Label 校验

Agent 退出后，serve 层对比执行前后的 label 快照，检测是否发生合法状态迁移。异常时记录 audit 事件和 report，不自动代改标签，保持 Agent-as-Router 语义。

详见：`docs/implemented/runtime-session-architecture.md` "Agent 退出后的 label 校验" 一节

### 5. command 字段状态

当前 `command` 与 `prompt` + `policy` 并存（v0.1.x 行为）。Runtime 优先读 `prompt`，无则读 `command`。未来版本将逐步 deprecate `command`。

## 尚未实施（v0.3.0+ 规划）

以下能力保留为未来目标，不在当前代码中：

- 长驻 runtime（`codex-appserver` 等）
- Per-repo app-server 连接池
- 动态审批回调
- 实时 UI 与事件回放

## 主要代码

- `internal/launcher/types.go` — Runtime/Session 接口定义
- `internal/launcher/process.go` — Session 实现
- `internal/launcher/events/` — Event Schema v1
- `internal/launcher/claude_stream.go` — Claude Code runtime
- `internal/launcher/codex.go` — Codex runtime
- `internal/launcher/output_contract.go` — JSON Schema 校验
- `internal/labelcheck/labelcheck.go` — Label 校验逻辑
