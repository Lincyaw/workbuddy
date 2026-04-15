# Roadmap and Structure Drift

状态：mismatch

## 现象

旧 roadmap 里有不少内容仍然有参考价值，但它已经不能直接代表当前仓库结构。

## 主要差异

### 1. 模块路径已经演进

旧 roadmap 中的产出路径和当前代码并不完全一致，例如：

- runtime 实现在 `internal/launcher/`，不是旧文本里的 `internal/worker/launcher.go`
- session 审计已经在 `internal/audit/`
- session UI 已经在 `internal/webui/`
- workspace isolation 已经在 `internal/workspace/`

### 2. 当前 CLI 范围远小于旧 roadmap 和 broad design

当前只有：

- `workbuddy serve`

而不是 roadmap/design 中列出的完整命令家族。

### 3. 一些 roadmap 中的“未来项”已经落地，但还停留在旧计划表述里

例如：

- SQLite store
- event log
- registry
- session audit
- web UI
- worktree isolation

这些内容现在应该放 implemented 文档，不应继续留在 roadmap 口径里。

## 处理原则

- 继续有现实价值的内容，迁移到 `implemented` 或 `planned`。
- 只保留真正还需要对齐的结构差异在 `mismatch`。
- 不再维护“旧 roadmap 原文 + 修修补补注释”的模式。
