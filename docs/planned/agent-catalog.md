# Agent Catalog

状态：planned

## 目的

Agent 抽象（`docs/planned/agent-schema-vnext.md`）落地后，可以批量定义不同 role 的 agent。这份文档登记**已规划但尚未实现**的 agent 类型，作为开 issue 的索引。

每个 agent 条目登记：

- role：在状态机中的位置
- 推荐 runtime
- 触发 label
- prompt 骨架要点
- 期望 output_contract（如有）

## 已实现

| 名称 | role | runtime | 状态 |
| --- | --- | --- | --- |
| `dev-agent` | dev | claude-code | ✅ |
| `test-agent` | test | claude-code | ✅ |
| `review-agent` | review | claude-code | ✅ |
| `codex-dev-agent` | dev | codex | ✅（注意 mismatch） |

## 规划中（按优先级排序）

### codex-test-agent

- role：test
- runtime：codex-appserver（首选）/ codex-exec（fallback）
- 触发：`status:testing`
- 行为：拉 PR 跑测试套件，失败回 `status:developing` 并 comment 失败原因；通过转 `status:reviewing`
- output_contract：`{passed: bool, failed_tests: [], coverage: float, notes: string}`
- 依赖：`event-schema-v1`、`runtime-session-architecture`

### codex-review-agent

- role：review
- runtime：codex-appserver
- 触发：`status:reviewing`
- 行为：跑 `codex review` 子命令做 PR review，结论决定 `status:done` 或 `status:developing`
- output_contract：`{approved: bool, blocking_issues: [], suggestions: []}`
- 依赖：codex `review/start` RPC（已在 schema 里）

### triage-agent

- role：triage
- runtime：claude-oneshot（轻量任务，oneshot 即可）
- 触发：issue `opened` 事件，无 status 标签时
- 行为：读 issue 内容，决定 type:feature / type:bug / type:question，加 `status:triage` → `status:developing`，必要时 ping 责任人
- output_contract：`{type: string, priority: string, assignee: string?, needs_clarification: bool}`

### docs-agent

- role：docs
- runtime：claude-oneshot
- 触发：label `type:docs` + `status:developing`
- 行为：根据 issue 描述更新 docs/、README、CLAUDE.md；不写代码
- 限制：`policy.sandbox: workspace-write`，禁止 `gh` 之外的网络

### security-audit-agent

- role：security
- runtime：codex-appserver（需要长会话和反复 grep）
- 触发：label `type:security`
- 行为：对指定模块跑安全审计，输出威胁清单 + 修复建议；不直接改代码，开子 issue
- output_contract：`{findings: [{severity, location, description, recommendation}]}`

### dependency-bump-agent

- role：deps
- runtime：codex-exec
- 触发：定时任务（不走 issue label，由 cron 触发） / label `type:deps`
- 行为：升级 go.mod、跑测试、开 PR
- 注：需要 cron 触发器支持，依赖未来 `workbuddy schedule` 能力

### release-agent

- role：release
- runtime：codex-appserver
- 触发：label `type:release`
- 行为：聚合 changelog、打 tag、生成 release notes
- output_contract：`{version: string, changelog_md: string, breaking_changes: []}`

## 共享 prompt 片段

为避免每个 agent 重复写 routing 指令，规划一个 `prompt_includes/` 目录存放可复用片段：

- `routing-dev-to-test.md`
- `routing-test-to-review.md`
- `routing-review-to-done.md`
- `pr-creation-guidelines.md`
- `commit-message-style.md`
- `gh-cli-cheatsheet.md`

agent md 通过 `prompt_includes: [routing-dev-to-test, pr-creation-guidelines]` 引用。具体加载机制留 vNext 实现时定。

## 维护规则

- 新增 agent 设计先在这里登记，不要直接写 `.github/workbuddy/agents/`
- 实现并合并后，把条目从"规划中"移到"已实现"
- 已实现条目如有 mismatch，在备注列标注并新建 mismatch 文档
