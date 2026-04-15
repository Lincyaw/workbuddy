# Agent Catalog

状态：implemented

## 目的

仓库样例现在把 catalog 中规划的 agent 类型全部落成了具体 agent 定义，统一放在
`.github/workbuddy/agents/` 下，并使用 vNext schema 的 `prompt`、`policy`、`output_contract`
字段。

## 当前已登记的 Agent

| 名称 | role | runtime | 主触发 | output_contract |
| --- | --- | --- | --- | --- |
| `dev-agent` | `dev` | `claude-code` | `status:developing` | 无 |
| `review-agent` | `review` | `claude-code` | `status:reviewing` | 无 |
| `codex-dev-agent` | `dev` | `codex` | `status:developing` | 无 |
| `codex-review-agent` | `review` | `codex` | `status:reviewing` | 无 |
| `triage-agent` | `triage` | `claude-oneshot` | `issue_created` | `schemas/triage-agent-result.json` |
| `docs-agent` | `docs` | `claude-oneshot` | `type:docs` | `schemas/docs-agent-result.json` |
| `security-audit-agent` | `security` | `codex-appserver` | `type:security` | `schemas/security-audit-agent-result.json` |
| `dependency-bump-agent` | `deps` | `codex` | `type:deps` | `schemas/dependency-bump-agent-result.json` |
| `release-agent` | `release` | `codex-appserver` | `type:release` | `schemas/release-agent-result.json` |

## 当前落地边界

- catalog 中每个 agent 都已经有对应的 `.md` 定义文件，可被 loader 直接读取。
- 带结构化结果的 agent 都声明了本地 JSON Schema，loader 会在启动时校验 schema 文件存在。
- `dependency-bump-agent` 目前先通过 `type:deps` label 注册；catalog 中提到的 cron 触发仍属于未来调度能力。
- `docs-agent` 的目标语义是文档专用执行；当前 sample config 用单一 label `type:docs` 作为主触发入口。
- `security-audit-agent` 与 `release-agent` 已登记为 `codex-appserver` runtime key，catalog 级配置先按 vNext schema 固化。

## 相关文件

- `.github/workbuddy/agents/dev-agent.md`
- `.github/workbuddy/agents/review-agent.md`
- `.github/workbuddy/agents/codex-dev-agent.md`
- `.github/workbuddy/agents/codex-review-agent.md`
- `.github/workbuddy/agents/triage-agent.md`
- `.github/workbuddy/agents/docs-agent.md`
- `.github/workbuddy/agents/security-audit-agent.md`
- `.github/workbuddy/agents/dependency-bump-agent.md`
- `.github/workbuddy/agents/release-agent.md`
- `.github/workbuddy/agents/schemas/`
- `internal/config/loader.go`
- `internal/config/types.go`
