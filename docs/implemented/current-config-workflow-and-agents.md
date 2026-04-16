# Current Config, Workflow, and Agents

状态：implemented

## 当前配置目录

当前 loader 读取的目录固定是 `.github/workbuddy/`，主要包含：

- `config.yaml`
- `agents/*.md`
- `workflows/*.md`

代码：

- `internal/config/loader.go`
- `internal/config/types.go`

## 当前 GlobalConfig 真正支持的字段

当前 `GlobalConfig` 只解析这几个顶层字段：

- `repo`
- `environment`
- `poll_interval`
- `port`
- `notifications`

代码：

- `.github/workbuddy/config.yaml`
- `internal/config/types.go`
- `internal/config/loader.go`

仓库样例 `.github/workbuddy/config.yaml` 还包含 `notifications` 配置块（用于告警路由），并在该块内包含四个可选通道的开关与环境变量名。

## 当前告警（notifications）配置

`notifications` 顶层字段用于配置告警总开关、实例名、去重窗口、批量窗口与告警渠道：

- `enabled`：总开关（默认 `false`；开启时才会执行外发）
- `instance_name`：告警实例标识
- `dedup_window`：同一 `(repo, issue_num, event_kind)` 的去重窗口
- `batch_window`：批量窗口（`0s` 时关闭批量，默认不批量）
- `success`：是否通知成功任务（默认 `false`）

渠道字段（均为环境变量名，仅存 `xxx_env`）：

- `slack.webhook_url_env`
- `feishu.webhook_url_env`
- `telegram.bot_token_env`
- `telegram.chat_id_env`
- `smtp.host_env`
- `smtp.port_env`
- `smtp.username_env`
- `smtp.password_env`
- `smtp.from_env`
- `smtp.to_env`

代码：

- `internal/config/types.go`
- `.github/workbuddy/config.yaml`
- `internal/notifier/notifier.go`
- `internal/config/loader.go`

## 当前 Agent Schema

当前 agent frontmatter 的有效字段：

- `name`
- `description`
- `triggers`
- `role`
- `runner`
- `runtime`
- `github_actions`
- `policy`
- `prompt`
- `output_contract`（历史字段，2-agent catalog 不再使用）
- `command`（legacy shim）
- `timeout`

当前 runtime 公共值：

- `claude-code`
- `codex`
- `codex-appserver`

默认行为：

- `runner` 为空时默认走 `local`
- `runtime` 为空时默认走 `claude-code`
- `prompt` 存在时 runtime 优先消费 `prompt`
- `command` 保留为兼容字段，在没有 `prompt` 时继续作为执行入口
- `output_contract.schema_file` 仍在 loader / launcher 中保留以兼容历史数据，
  但内置的 2 个 agent 都不再声明此字段

当 `runner: github-actions` 时，可选 `github_actions` 字段：

- `workflow`
- `ref`
- `poll_interval`

代码：

- `internal/config/types.go`
- `internal/config/loader.go`
- `internal/launcher/launcher.go`
- `internal/launcher/output_contract.go`

## 当前 Workflow Schema

当前 workflow frontmatter 支持：

- `name`
- `description`
- `trigger.issue_label`
- `max_retries`

Markdown 正文中，loader 会取第一个 fenced `yaml` block 解析 `states`：

- `enter_label`
- `agent`
- `action`
- `transitions[].to`
- `transitions[].when`

代码：

- `internal/config/loader.go`
- `internal/config/types.go`

## 当前状态机

状态机只剩 4 个节点：

```
           ┌──────────────┐       ┌──────────────┐
issue ───▶ │  developing  │ ─────▶│   reviewing  │ ──▶ done
           └──────┬───────┘       └───────┬──────┘
                  │  ▲                    │
                  │  └────────────────────┘   (review 反馈失败，回到 developing)
                  │  ▲
                  ▼  │ (human 补齐 criteria 后改回 status:developing)
              ┌──────────┐
              │ blocked  │
              └──────────┘
```

- `status:developing` → dev-agent 产出工件 → `status:reviewing`
- `status:reviewing` → review-agent 评估 → `status:done` 或回到 `status:developing`
- `status:blocked` 是 dev-agent 在发现 `## Acceptance Criteria` 缺失时打的等待标签；
  人工补齐后把 label 改回 `status:developing` 继续流转（非终态）
- `status:done` 是终态

示例 workflow yaml（按新 catalog 写）：

```yaml
states:
  - enter_label: status:developing
    agent: dev-agent
    transitions:
      - when: labeled "status:reviewing"
        to: reviewing
      - when: labeled "status:blocked"
        to: blocked
  - enter_label: status:reviewing
    agent: review-agent
    transitions:
      - when: labeled "status:done"
        to: done
      - when: labeled "status:developing"
        to: developing
```

## 当前工作流触发方式

当前真正生效的是 label 驱动模型：

- issue 带上 workflow trigger label 后，该 workflow 才参与匹配
- state machine 通过当前 issue labels 匹配 `enter_label` 推断当前状态
- 当某个状态 label 被新增，且该状态配置了 `agent`，就触发 dispatch

关键代码：

- `internal/statemachine/statemachine.go`
- `.github/workbuddy/workflows/default.md`

## 当前 Poller 事件集合

目前 Poller 只会发出以下事件：

- `issue_created`
- `label_added`
- `label_removed`
- `pr_created`
- `pr_state_changed`
- `issue_closed`

代码：

- `internal/poller/poller.go`

## 当前 Agent 与 Workflow 的实际协作方式

以默认仓库样例看，事实是：

- 只有 `dev-agent` 和 `review-agent` 两个内置 agent
- dev-agent 读 issue 的 `## Acceptance Criteria`，缺失直接打 `status:blocked`，
  否则产出工件并翻到 `status:reviewing`
- review-agent 评估工件是否满足每条验收标准，通过 → `status:done`，
  不过 → 回 `status:developing` 并写评审评论
- 测试由 dev-agent 作为工件的一部分产出、由 review-agent 验证，不设独立的 test-agent
- workflow 用 `when: labeled "status:..."` 表达状态切换
- 状态推进靠 agent 自己执行 `gh issue edit`
- Go 侧只观察结果，不替 agent 改 label
- 仓库相关的 dev-loop 命令（`go build` / `go test` / lint）由业务仓库自己的
  `CLAUDE.md` 和 `.claude/skills/` 提供，workbuddy 的 agent prompt 中不再硬编码

相关文件：

- `.github/workbuddy/agents/dev-agent.md`
- `.github/workbuddy/agents/review-agent.md`
- `.github/workbuddy/workflows/default.md`
- `docs/decisions/2026-04-15-agent-role-consolidation.md`
