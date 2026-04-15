# Current Runtime, Reporting, and Audit

状态：implemented

## 当前 Runtime 抽象

当前 launcher 仍然是一次性执行模型：

```go
type Runtime interface {
    Name() string
    Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error)
}
```

代码：

- `internal/launcher/types.go`

这代表当前没有统一的 `Session` 抽象，也没有标准化 event stream。

## 当前内建 runtime

当前注册和使用的 runtime 只有两类：

- `claude-code`
- `codex`

相关代码：

- `internal/launcher/launcher.go`
- `internal/launcher/claude.go`
- `internal/launcher/codex.go`
- `internal/launcher/process.go`

## 当前 Runtime 共同行为

当前两类 runtime 都遵循 one-shot 子进程模型：

- 渲染 agent `command` 模板
- 在 task workdir 下启动子进程
- 用 context 和 timeout 控制生命周期
- 一次性收集 stdout/stderr
- 尝试从 stdout 中抽取 `WORKBUDDY_META`
- 尝试记录 session 文件或 last-message 文件路径

这部分结果最终仍然汇总到 `launcher.Result`，而不是 streaming event。

## 当前 Reporter 行为

Reporter 负责向 GitHub issue 写评论，当前主要有两类输出：

- 开始执行时的 started comment
- 执行结束后的结果 comment

它依赖的信息主要来自：

- `launcher.Result`
- session id
- worker id
- retry 次数

代码：

- `internal/reporter/reporter.go`
- `internal/reporter/format.go`

## 当前 Audit 行为

Auditor 会把 session 产物归档到磁盘，并把摘要写入 SQLite：

- Claude 类会话优先按 JSON session 做摘要
- Codex 类会话优先按日志文本提炼关键行
- 若没有 session 文件，就退化为 stdout/stderr 摘要

代码：

- `internal/audit/audit.go`
- `internal/store/store.go`

## 当前 Sessions Web UI

当前已经有 `/sessions` 页面：

- 可按 repo、agent、issue 过滤
- 可以查看单个 session 详情
- 详情页会联动 task 状态

代码：

- `internal/webui/handler.go`

## 当前限制

这些都仍然是当前真实限制：

- reporter 消费的是最终结果，不是实时事件流
- audit 虽然保存了 session 归档，但并没有统一 Event v1
- codex 的 `--json` 输出还没有被提升为统一结构化事件模型
- approval 还不是 runtime/session 层的一等接口
