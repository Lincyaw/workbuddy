# Codex Runtime Drift

状态：archived

该 drift 已由 issue #8 收敛：

- `internal/launcher` 已升级为 `Runtime.Start -> Session.Run`
- `internal/launcher/events/` 已落地 Event Schema v1
- `internal/agent/codex/backend.go` 已按 `codex app-server` JSON-RPC 实时映射事件
- `Result.LastMessage` / `Result.SessionRef` / `Result.TokenUsage` 已接入 reporter / audit
- session 事件已写入 `.workbuddy/sessions/<session>/` 下的 artifact

因此这份 mismatch 文档只保留作归档说明，不再代表当前代码现状。
