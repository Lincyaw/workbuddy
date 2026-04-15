# Runtime Session Architecture

状态：implemented

## 当前抽象

当前 launcher 已经把执行单位从一次性 `Launch(...)` 升级为 `Start(...) -> Session.Run(...)`：

```go
type Runtime interface {
    Name() string
    Start(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error)
    Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error)
}

type Session interface {
    Run(ctx context.Context, events chan<- Event) (*Result, error)
    SetApprover(Approver) error
    Close() error
}
```

当前事实：

- 调用方可以直接拿到 `Session`，并把统一事件流写到外部 channel。
- `Launch(...)` 只是对 `Start(...) + Session.Run(...)` 的兼容包装。
- Claude 和 Codex 的 one-shot runtime 都已经接到同一套 Session 接口上。

相关代码：

- `internal/launcher/types.go`
- `internal/launcher/launcher.go`
- `internal/launcher/claude.go`
- `internal/launcher/codex.go`
- `internal/launcher/process.go`

## 当前主链路

当前 `serve` 主链路已经是：

```text
Router -> Runtime.Start -> Session.Run -> Event stream -> Reporter / Audit / Web UI
```

在 `cmd/serve.go` 的 worker 路径里，`executeTask(...)` 会：

1. `launcher.Start(...)` 拿到 `Session`
2. 启动 Event Schema v1 事件采集
3. 在 `Session.Run(...)` 前后各做一次 issue label snapshot
4. 用 `internal/labelcheck` 对 pre/post label 做分类
5. 把统一 event artifact、label 校验结果、最终 `Result` 一起交给 reporter/audit

这条链路现在已经是统一行为，不再要求 reporter / audit 各自解析 runtime 私有输出。

相关代码：

- `cmd/serve.go`
- `internal/labelcheck/labelcheck.go`
- `internal/reporter/reporter.go`
- `internal/audit/audit.go`

## Result 与 Approver

当前 `launcher.Result` 已经承载 runtime 结束态需要的共同字段：

- `Stdout` / `Stderr`
- `LastMessage`
- `TokenUsage`
- `SessionPath`
- `RawSessionPath`
- `SessionRef`

`Approver` 接口也已经冻结在 launcher 层，但当前内建 runtime 仍然返回 `ErrNotSupported`；这代表接口形状已落地，动态审批本身还没有接入执行后端。

## Agent 退出后的 label 校验

当前 Go 侧已经具备 post-Run label validation，但仍然严格停留在现有 GH read/write boundary 内：

- 只读：`gh issue view --json labels`
- 只写：`gh issue comment`
- 不会由 Go 侧执行 `gh issue edit` 代改 label

当前行为：

| 情形 | 当前处理 |
| --- | --- |
| label 变到当前 state 合法 `transitions:` 对应的目标 label | 记录 `label.validation` 审计事件，report comment 显示 `Label transition: ... (OK)` |
| label 无变化且 `ExitCode == 0` | 记录 `label.validation` 审计事件；额外发一条 managed comment，建议人工加 `needs-human`；不由 Go 侧重试/改 label |
| label 无变化且 `ExitCode != 0` | 记录 `label.validation` 审计事件；保留现有 failure / retry 路径 |
| label 变到不在当前 state `transitions:` 中的目标 | 记录 `label.validation` 审计事件，分类为 `unexpected_transition`；不阻断后续按实际 label 消费 |
| post labels 含 `status:failed` | 记录 `label.validation` 审计事件，分类为 `failed`；保留现有 failed 流程 |

审计 payload 形状固定为：

```json
{
  "pre": ["workbuddy", "status:developing"],
  "post": ["workbuddy", "status:reviewing"],
  "exit_code": 0,
  "classification": "ok"
}
```

相关代码：

- `cmd/serve.go`
- `internal/labelcheck/labelcheck.go`
- `internal/audit/audit.go`
- `internal/reporter/format.go`

## 当前边界

这份文档现在只记录已经落地的 Session 架构和 label 校验行为。

仍然属于 future work 的内容，例如：

- long-lived runtime / app-server 连接池
- per-repo runtime 复用与 idle 回收
- 动态审批回调
- distributed worker / coordinator transport

继续放在 `docs/planned/runtime-migration-plan.md` 和其它 planned 文档里维护。
