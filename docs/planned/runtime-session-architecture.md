# Runtime Session Architecture

状态：planned

## 为什么要升级抽象

当前 `internal/launcher` 的接口只覆盖一次性执行：

```go
type Runtime interface {
    Name() string
    Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error)
}
```

它适合 today 的 one-shot CLI，但不能自然覆盖：

- Claude one-shot 的流式输出
- `codex exec --json` 的 JSONL 事件
- 长驻 `codex app-server`
- 未来的 RPC / HTTP / MCP 型 runtime

## 目标分层

目标是把执行单位从“一次调用”升级成“一次 session”：

```text
Router -> Runtime -> Session -> Event stream -> Reporter/Audit/UI
```

设计原则：

- Runtime 负责启动后端
- Session 代表一次 agent 执行
- Event stream 是一等公民
- Reporter、Audit、Web UI 不再各自解析不同 runtime 的私有输出

## 目标接口

建议接口：

```go
type Runtime interface {
    Name() string
    Start(ctx context.Context, agent *Agent, task *TaskContext) (Session, error)
}

type Session interface {
    Run(ctx context.Context, events chan<- Event) (*Result, error)
    SetApprover(Approver) error
    Close() error
}
```

附带约束：

- `Run` 推进 session 到终态
- `events` 由调用方持有并关闭
- `Close` 必须幂等
- 不支持动态审批的 runtime 对 `SetApprover` 返回 `ErrNotSupported`

## Result 目标演进

目标 `Result` 需要兼容现状，也要支撑 richer runtime：

- 保留 `Stdout` / `Stderr`
- 新增 `LastMessage`
- 新增 `TokenUsage`
- 新增 `SessionRef`
- 可选 `Diff`

这样 reporter/audit 可以优先消费统一字段，而不是 runtime 私有文本。

## Approver 预留

目标接口里会预留审批：

- v0.1.x 可先只实现 `AlwaysAllow`
- 后续可扩展 `PolicyBased` 或人类审批

但 approval 预留不应阻塞当前抽象迁移。

### Approver 接口形状

即便 v0.1.x 只实现 `AlwaysAllow`，方法签名也要现在锁住，避免 v0.2 接 codex-appserver 时回头讨论：

```go
type Approver interface {
    Approve(ctx context.Context, req ApprovalRequest) ApprovalDecision
}

type ApprovalRequest struct {
    Kind   ApprovalKind    // 见枚举
    Detail json.RawMessage // runtime 原生 payload，原样透传
    Source SessionRef      // 触发请求的 session 来源
}

type ApprovalKind string
const (
    ApprovalExec        ApprovalKind = "exec"          // 执行 shell / git / gh
    ApprovalPatch       ApprovalKind = "patch"         // 写文件 / apply diff
    ApprovalPermissions ApprovalKind = "permissions"   // 提升沙箱权限
    ApprovalToolInput   ApprovalKind = "tool_input"    // 工具要求补输入
    ApprovalMCPElicit   ApprovalKind = "mcp_elicit"    // MCP 子工具 elicitation
)

type ApprovalDecision struct {
    Allow  bool
    Scope  ApprovalScope  // once | session | forever
    Reason string         // 写入 audit
}

type ApprovalScope string
const (
    ScopeOnce    ApprovalScope = "once"
    ScopeSession ApprovalScope = "session"
    ScopeForever ApprovalScope = "forever"
)
```

`SetApprover` 在不支持动态审批的 runtime（claude-oneshot / codex-exec）上返回 `ErrNotSupported`，但接口本身在 v1 里就冻住，不再变更。

## Long-lived runtime 生命周期

`codex-appserver` 等长驻 runtime 的进程模型与 one-shot runtime 不同，必须明确隔离单位与回收策略。

### 隔离单位：per-repo

- 一个 long-lived runtime 进程对应**一个 repo full name**（如 `Lincyaw/workbuddy`）。
- 多个 issue 共享同一个 repo 的 runtime 进程，但每个 task 起独立 `thread`。
- repo 之间完全隔离：进程隔离 + cwd 隔离（与 worktree 单位对齐） + rate limit 隔离 + config 隔离。

### 启停策略

- **懒启动**：首次该 repo 有 task 时拉起进程，握手并缓存连接。
- **空闲回收**：可配置 `runtime.idle_timeout`（默认 10m），无活跃 thread 且无新 task 超过该时长时优雅关闭。
- **健康检查**：协程定期 ping `model/list` 或等价轻量 RPC，失败则销毁连接，下次 task 触发重建。
- **强制重启**：检测到协议 schema 版本与启动时不一致（codex 升级）时回收。

### 与 worktree 的对齐

worktree 的隔离单位也是 per-repo per-task，但 long-lived runtime 进程跨 task 复用。约束：

- 进程层面 cwd 不固定，每个 `thread/start` 显式传 `cwd: <worktree-path>`。
- worktree 销毁前必须先 `thread/archive` 或 `thread/unsubscribe`，避免 RPC 引用悬挂目录。
- workspace.Manager 与 runtime pool 共享生命周期事件（task 完成 → 通知双方）。

### 共享与隔离边界总结

| 维度 | 同 repo 多 task | 不同 repo |
|---|---|---|
| 进程 | 共享 | 隔离 |
| thread | 隔离 | 隔离 |
| cwd / worktree | 隔离 | 隔离 |
| rate limit | 共享（同账号） | 共享（同账号） |
| codex config | 共享 | 共享 |

## Agent 退出后的 label 校验

Go 侧不代改 label，但要兜住 agent 漏改/错改的情况。校验流程：

1. `Session.Run` 启动前，记录 issue 的 label snapshot（pre-labels）。
2. `Run` 返回后，重新 `gh issue view --json labels` 拉 post-labels。
3. 比较 diff，按下列规则归档：

| 情形 | 处理 |
|---|---|
| label 出现合法转移（在当前 state 的 `transitions:` 中） | OK，状态机推进 |
| label 完全无变化 且 `Result.ExitCode == 0` | 警告，加 `action:needs-human`，留在原 state，**不重试** |
| label 完全无变化 且 `Result.ExitCode != 0` | 走 retry 策略；超过 max_retries → `status:failed` |
| label 变到不在当前 state `transitions:` 中的值 | 记录 audit 事件 `label.unexpected_transition`，**不阻断**（保留 agent 决策权），按 label 实际值进入新状态 |
| label 出现 `status:failed` | 直接进 failed 流程 |

校验本身不改动 label，所有动作都是 audit 记录 + 状态机入参。这条规则适用于所有 runtime（oneshot 或 long-lived），不区分实现。
