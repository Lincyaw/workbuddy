# Artifact Layout

状态：implemented

## 背景

当前 session artifact 已统一写到仓库根 `.workbuddy/sessions/session-<sid>/`。
`internal/launcher/codex.go` 的 runtime 原始产物和 `cmd/serve.go:streamSessionEvents`
生成的 `events-v1.jsonl` 都不再依赖 `task.WorkDir`，因此 worktree 清理不会删除它们。

## 当前行为

给 workbuddy 的存储分三层，路径各司其职，互不踩踏：

| 层级 | 路径 | 内容 | 生命周期 |
| --- | --- | --- | --- |
| 工作区 | `<repoRoot>/.workbuddy/worktrees/<issue>-<taskShort>/` | git worktree、agent 的临时分支 | task 结束即清理 |
| Session artifact | `<repoRoot>/.workbuddy/sessions/session-<sid>/` | 事件流、last-message、auditor summary | 跟随仓库；`.gitignore` 内 |
| Coordinator 运行时 | `~/.workbuddy/`（v0.2+） | 全局 DB、log、registry、credentials | 跨仓库长期保留 |

当前实现只覆盖前两层；第三层作为 v0.2.x 分布式拓扑的一部分，见
`docs/planned/distributed-topology-and-cli.md`。

## 实现

### 1. Session artifact 统一写到仓库根

原则：**launcher 写的 artifact 路径不依赖 `task.WorkDir`**。
worktree 只装 agent 改的代码，不再装事件流。

1. `internal/launcher/types.go`：`TaskContext` 增加 `RepoRoot string` 字段，语义为
   "main worktree / coordinator 启动时的仓库根"。
2. `internal/router/router.go`：构造 `TaskContext` 时填入 `RepoRoot = repoDir`。
   `cmd/run.go` 的直接运行路径也把 `RepoRoot` 设为当前工作目录。
3. `internal/launcher/codex.go:newCodexSession`：artifact baseDir 优先取 `task.RepoRoot`，
   为空时回退到 `WorkDir` 以兼容现有测试和简化调用方。
4. `cmd/serve.go:streamSessionEvents`：统一通过 `sessionArtifactsBaseDir(...)` 把
   `events-v1.jsonl` 写到 `<RepoRoot>/.workbuddy/sessions/session-<sid>/`。

### 2. 错误路径下 artifact 不丢

由于 artifact 已经不在 worktree 里，worktree 清理不会再把它删掉。
`cmd/serve.go:executeTask` 的行为是：

- 无论 runtime 是否返错，只要 `session.Run(...)` 返回了非 nil `Result`（含
  `SessionPath`、`LastMessage`、`TokenUsage`），都在 return 前先调 `audit.Capture(...)`。
- 当 `events-v1.jsonl` 成功落盘时，它会覆盖 `Result.SessionPath` 成为 canonical artifact；
  runtime 原始路径保存在 `Result.RawSessionPath`。

### 3. 目录布局约定

```
<repoRoot>/
├── .workbuddy/
│   ├── workbuddy.db              # SQLite
│   ├── logs/                     # coordinator 日志
│   ├── sessions/
│   │   └── session-<sid>/
│   │       ├── events-v1.jsonl   # workbuddy 统一 Event Schema v1
│   │       ├── codex-exec.jsonl   # runtime 原始 JSONL（仅 codex-exec）
│   │       ├── codex-last-message.txt
│   │       └── ...               # auditor 归档/复制出的文件
│   └── worktrees/
│       └── issue-<N>-<taskShort>/  # 仅代码和临时分支
└── .gitignore  # 追加 .workbuddy/
```

### 4. 兼容性

- `TaskContext.RepoRoot` 为空时回退到 `WorkDir`（本地单元测试场景），保持现有
  测试可跑。
- artifact 迁移不涉及磁盘 schema 变更，旧 session 目录可以直接保留，不需要迁移脚本。
- 不影响 `runtime: codex` → `codex-exec` 的既有 normalize 逻辑。

## 验证结果

- Codex/Claude 两种 runtime 的 session artifact 均写在 `<repoRoot>/.workbuddy/sessions/session-<sid>/`，
  worktree 内不再出现 `.workbuddy/sessions/`。
- worker 无论 runtime success/timeout/cancel，只要有 `Result`，`audit.Capture(...)` 都会被调用。
- 单元测试：给 `TaskContext.RepoRoot` 传任意临时目录，artifact 路径符合上表。
- 本地验证命令：`go test ./... -count=1`、`go vet ./...`、`go build ./...`。

## 不做（留给后续 issue）

- 迁移到 `~/.workbuddy/` 的全局 coordinator 存储（由 v0.2.x 分布式拓扑覆盖）。
- Session artifact 的异地归档 / 对象存储上传。
- auditor 的 schema 升级（本文只改路径，不改产出格式）。

## 相关设计

- 前置：Runtime / Session 抽象、Event Schema v1（都已 implemented）。
- 阻挡：无；可独立落地。
- 相关：`docs/planned/distributed-topology-and-cli.md`（全局 `~/.workbuddy/` 在那里讨论）。
