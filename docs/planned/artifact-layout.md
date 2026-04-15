# Artifact Layout（计划中）

状态：planned

## 背景

当前 session artifact（`codex-events.jsonl`、`events-v1.jsonl`、`codex-last-message.txt`）
由 `internal/launcher/codex.go` 和 `cmd/serve.go:streamSessionEvents` 以 `task.WorkDir`
为 baseDir 写入，因此最终落在 **每个任务的 git worktree 内部**：

```
.workbuddy/worktrees/issue-8-<taskShort>/.workbuddy/sessions/session-<sid>/
├── codex-events.jsonl
├── events-v1.jsonl
└── codex-last-message.txt
```

问题：

1. worktree 是 per-task 临时目录，任务结束后 `wsMgr.Remove(...)` 会被 defer 清理，原始事件流随之消失。
2. 在 `cmd/serve.go:executeTask` 的 runtime 错误返回路径上，worker 在 `audit.Capture()`
   被调用之前就 return，而 worktree 清理 defer 已注册 → 失败路径的 artifact 永远进不了 auditor。
3. auditor 自己另写一份到仓库根 `.workbuddy/sessions/session-<sid>/`，和 launcher 侧的
   artifact 目录名相同但物理路径不同，排障时极易混淆。

## 目标

给 workbuddy 的存储分三层，路径各司其职，互不踩踏：

| 层级 | 路径 | 内容 | 生命周期 |
| --- | --- | --- | --- |
| 工作区 | `<repoRoot>/.workbuddy/worktrees/<issue>-<taskShort>/` | git worktree、agent 的临时分支 | task 结束即清理 |
| Session artifact | `<repoRoot>/.workbuddy/sessions/session-<sid>/` | 事件流、last-message、auditor summary | 跟随仓库；`.gitignore` 内 |
| Coordinator 运行时 | `~/.workbuddy/`（v0.2+） | 全局 DB、log、registry、credentials | 跨仓库长期保留 |

本文档只规划前两层的收敛；第三层作为 v0.2.x 分布式拓扑的一部分，见
`docs/planned/distributed-topology-and-cli.md`。

## 设计

### 1. Session artifact 统一写到仓库根

原则：**launcher 写的 artifact 路径不依赖 `task.WorkDir`**。
worktree 只装 agent 改的代码，不再装事件流。

落地步骤：

1. `internal/launcher/types.go`：`TaskContext` 增加 `RepoRoot string` 字段，语义为
   "main worktree / coordinator 启动时的仓库根"。
2. `cmd/serve.go`：在构造 `WorkerTask` 时填入 `RepoRoot = repoDir`（即当前
   `workspace.NewManager(repoDir)` 的 `repoDir`）。
3. `internal/launcher/codex.go:newCodexSession`：把 `baseDir` 从 `task.WorkDir` 改成
   `task.RepoRoot`，artifact 目录变成 `<RepoRoot>/.workbuddy/sessions/session-<sid>/`。
4. `cmd/serve.go:streamSessionEvents`：同样把 baseDir 改成 `task.RepoRoot`。
5. auditor 已经用 `.workbuddy/sessions`，目录命名一致后可以直接复用（不再复制一份）。

### 2. 错误路径下 artifact 不丢

由于 artifact 已经不在 worktree 里，worktree 清理不会再把它删掉。
在此基础上，`cmd/serve.go:executeTask` 进一步：

- 无论 runtime 是否返错，只要 `session.Run(...)` 返回了非 nil `Result`（含
  `SessionPath`、`LastMessage`、`TokenUsage`），都在 return 前先调 `audit.Capture(...)`。
- audit 的 summary 写在同一个 session-id 目录下，不再另开新目录。

### 3. 目录布局约定

```
<repoRoot>/
├── .workbuddy/
│   ├── workbuddy.db              # SQLite
│   ├── logs/                     # coordinator 日志
│   ├── sessions/
│   │   └── session-<sid>/
│   │       ├── events-v1.jsonl   # workbuddy 统一 Event Schema v1
│   │       ├── codex-events.jsonl  # runtime 原始 JSONL（仅 codex-exec）
│   │       ├── codex-last-message.txt
│   │       └── audit-summary.md  # auditor 归档
│   └── worktrees/
│       └── issue-<N>-<taskShort>/  # 仅代码和临时分支
└── .gitignore  # 追加 .workbuddy/
```

### 4. 兼容性

- `TaskContext.RepoRoot` 为空时回退到 `WorkDir`（本地单元测试场景），保持现有
  测试可跑。
- artifact 迁移不涉及磁盘 schema 变更，旧 session 目录可以直接保留，不需要迁移脚本。
- 不影响 `runtime: codex` → `codex-exec` 的既有 normalize 逻辑。

## 与当前代码的距离

| 项 | 现状 | 目标 |
| --- | --- | --- |
| codex session artifact baseDir | `task.WorkDir`（worktree 内）| `task.RepoRoot`（仓库根）|
| stream events baseDir | `task.WorkDir` 或 CWD | `task.RepoRoot` |
| auditor vs launcher 路径 | 命名一致但物理路径两份 | 物理路径归并到同一 `sessions/session-<sid>/` |
| 失败路径 artifact | worktree 随 defer 清理丢失 | 保留在仓库根 sessions 下 |
| TaskContext 字段 | 仅 `WorkDir` | `WorkDir` + `RepoRoot` |

## 验收标准

- Codex/Claude 两种 runtime 的 session artifact 均写在 `<repoRoot>/.workbuddy/sessions/session-<sid>/`，
  worktree 内不再出现 `.workbuddy/sessions/`。
- worker 无论 runtime success/timeout/cancel，只要有 `Result`，`audit.Capture(...)` 都会被调用。
- 单元测试：给 `TaskContext.RepoRoot` 传任意临时目录，artifact 路径符合上表。
- 集成验证：跑完一次 dev→test→review 闭环，review/test/dev 三个 session 的 jsonl
  都在同一仓库根 `sessions/` 下，worktree 已清空但 artifact 仍在。

## 不做（留给后续 issue）

- 迁移到 `~/.workbuddy/` 的全局 coordinator 存储（由 v0.2.x 分布式拓扑覆盖）。
- Session artifact 的异地归档 / 对象存储上传。
- auditor 的 schema 升级（本文只改路径，不改产出格式）。

## 依赖关系

- 前置：Runtime / Session 抽象、Event Schema v1（都已 implemented）。
- 阻挡：无；可独立落地。
- 相关：`docs/planned/distributed-topology-and-cli.md`（全局 `~/.workbuddy/` 在那里讨论）。
