# Agent Catalog

状态：implemented

## 目的

Workbuddy 采用最小化的 2-agent catalog。每个 agent 的存在必须通过 "没有它会失去什么"
的检验。runtime、内容域、触发条件等都是实现细节或配置覆盖，而不是独立的 catalog 条目。

## 当前已登记的 Agent

| 名称 | role | runtime | 主触发 | policy | 契约 |
| --- | --- | --- | --- | --- | --- |
| `dev-agent` | `dev` | `claude-code`（可按仓库覆盖） | `status:developing` | `danger-full-access`，`timeout` 按仓库配置 | 读取 issue 的 `## Acceptance Criteria`；若缺失 → `status:blocked`；否则产出满足每条验收标准的工件（代码、文档、依赖升级、报告、release notes 等都算工件），然后把 label 翻到 `status:reviewing` |
| `review-agent` | `review` | `claude-code`（可按仓库覆盖） | `status:reviewing` | `danger-full-access`，`timeout` 按仓库配置 | 针对每条 acceptance criterion 评估工件：全部通过 → `status:done`；任一失败 → 回到 `status:developing` 并在 issue 中写明反馈 |

### dev-agent

- role：`dev`
- 触发 label：`status:developing`
- runtime：默认 `claude-code`；仓库可通过 `.github/workbuddy/config.yaml` 或 agent 配置覆盖，
  例如切换到 `codex`。runtime 是配置细节，不再各自占一个 catalog 条目。
- policy：默认 `danger-full-access`；sandbox / approval / model 将来由 Coordinator 根据 issue
  label 做动态分派（尚未实现，见决策记录）。
- 契约：工件形态不受限 —— 只要满足 `## Acceptance Criteria` 中的条目即可。
  dev-agent 负责产出测试（单测 / 集成 / 手工复现步骤），不存在独立的 test-agent。

### review-agent

- role：`review`
- 触发 label：`status:reviewing`
- runtime：默认 `claude-code`，覆盖方式同上。
- policy：默认 `danger-full-access`。review-agent 会在业务仓库内运行 dev-loop
  （`go build` / `go test` / 仓库自定义 check），具体命令由业务仓库的 `CLAUDE.md` 和
  `.claude/skills/` 决定，不写进 workbuddy 自身的 agent prompt 里。
- 契约：产出是非结构化的评审评论（走 `gh issue comment`），因此本 catalog 不再为
  review-agent 声明 output_contract。

## 已退役（retired）

以下 7 个 agent 随 2026-04-15 决策一起退役，参见
`docs/decisions/2026-04-15-agent-role-consolidation.md`：

retired: triage-agent, codex-dev-agent, codex-review-agent, docs-agent,
dependency-bump-agent, release-agent, security-audit-agent

简要理由：

- `codex-dev-agent` / `codex-review-agent` 只是 runtime 不同，runtime 下沉为
  `dev-agent` / `review-agent` 的配置覆盖。
- `docs-agent` / `dependency-bump-agent` / `release-agent` / `security-audit-agent`
  本质都是 "按验收标准产出工件"，属于 `dev-agent` 的职责，不再按内容域切分。
- `triage-agent` 所做的是读 issue 并猜验收标准 —— 这件事由人类在 issue 模板里直接写清楚，
  不再用 LLM 反推。

关于 schema 字段：历史上的 `output_contract` 字段（如 `schemas/triage-agent-result.json`
那类结构化结果声明）在新的 2-agent catalog 中不再使用，因为 dev-agent 的工件形态自由、
review-agent 的输出是自然语言评论。`output_contract` 字段本身在 loader 中保留以兼容历史数据，
参见 `docs/implemented/agent-schema-vnext.md`。

## 相关文件

- `.github/workbuddy/agents/dev-agent.md`
- `.github/workbuddy/agents/review-agent.md`
- `internal/config/loader.go`
- `internal/config/types.go`
- `docs/decisions/2026-04-15-agent-role-consolidation.md`
