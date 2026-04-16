# Current Runtime, Reporting, and Audit

状态：implemented

## 当前 Runtime / Session 抽象

当前 launcher 已经升级为 `Runtime.Start(...) -> Session.Run(...)`：

```go
type Runtime interface {
    Name() string
    Start(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error)
    Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error)
}
```

代码：

- `internal/launcher/types.go`

当前事实：

- 调用方可以直接拿 `Session`，并把统一事件流写到外部 channel
- `Launch(...)` 只是对 `Start(...) + Session.Run(...)` 的兼容包装
- Event Schema v1 已经在 launcher 层落地

## 当前内建 runtime

当前注册和使用的 runtime 只有两类：

- `claude-code`
- `codex`

相关代码：

- `internal/launcher/launcher.go`
- `internal/launcher/claude.go`
- `internal/launcher/codex.go`
- `internal/launcher/process.go`

此外，launcher 现在还有一个与 runtime 正交的 runner 选择：

- `local`
- `github-actions`

其中 `github-actions` runner 会通过 `gh api` 触发 workflow dispatch，
然后下载 logs/artifacts 并重新组装成 `launcher.Result`。

## 当前 Runtime 共同行为

当前 runtime 的共同行为是：

- 都暴露 `Start(...) -> Session`
- 都用 context 和 timeout 控制生命周期
- 都把 session artifact 放到 repo-root `.workbuddy/sessions/<session>/`
- 都返回统一的 `launcher.Result`

差异点：

- `codex` runtime 会实时把 `codex exec --json` 映射成 Event v1
- Claude prompt 路径会把 `claude --output-format stream-json` 映射成 Event v1
- 保底 shell one-shot 路径仍只产出最小事件集
- GitHub Actions runner 不在本机执行 agent 子进程；它把远端 logs/artifacts
  回收到账户下的 `.workbuddy/sessions/<session>/remote-runner/`

## 当前 Reporter 行为

Reporter 负责向 GitHub issue 写评论，当前主要有两类输出：

- 开始执行时的 started comment
- 执行结束后的结果 comment
- 在 `ExitCode == 0` 但 workflow label 没有变化时，再追加一条 managed comment，建议人工加 `needs-human`

它当前依赖的信息主要来自：

- `launcher.Result`
- session id
- worker id
- retry 次数
- post-Run label validation summary

Reporter 还没有直接消费实时 event stream，但在 `serve` 主链路里：

- `Result.LastMessage` 来自 runtime 解析结果
- `Result.SessionPath` 会优先指向归一化后的 `events-v1.jsonl`
- comment 里的 session link 会跳到同一份统一事件 artifact

代码：

- `internal/reporter/reporter.go`
- `internal/reporter/format.go`

## 当前 Audit 行为

Auditor 会把 session 产物归档到磁盘，并把摘要写入 SQLite：

- Claude 类会话优先按 JSON session 做摘要
- Event v1 artifact 优先按统一 schema 做摘要
- Codex 原生日志文本只作为旧路径 fallback
- 若没有 session 文件，就退化为 stdout/stderr 摘要
- 另外还会把 post-Run label validation 以 `label.validation` 事件写入 `events` 表

代码：

- `internal/audit/audit.go`
- `internal/store/store.go`

## 当前 Sessions Web UI

当前已经有 `/sessions` 页面：

- 可按 repo、agent、issue 过滤
- 可以查看单个 session 详情
- 详情页会联动 task 状态
- 若存在 `events-v1.jsonl`，可以查看分页事件 JSON 和 SSE stream

代码：

- `internal/webui/handler.go`

## 当前仍然存在的限制

- reporter 还不是事件流消费者，只消费最终 `Result`
- Claude 的结构化映射只覆盖 prompt / `claude -p` 路径；保底 shell 路径仍是最小事件集
- approval 接口已冻结在 launcher/session 层，但当前内建 runtime 仍返回 `ErrNotSupported`

相关文档：

- `docs/implemented/event-schema-v1.md`
