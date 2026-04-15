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

代码：

- `internal/config/types.go`

注意：`.github/workbuddy/config.yaml` 里虽然还写了 `server.port`、`agents_dir`、`workflows_dir`、`log`，但这些不属于当前实现事实，这部分差异已单独转到 `docs/mismatch/config-schema-drift.md`。

## 当前 Agent Schema

当前 agent frontmatter 的有效字段：

- `name`
- `description`
- `triggers`
- `role`
- `runtime`
- `command`
- `timeout`

当前 runtime 公共值：

- `claude-code`
- `codex`

默认行为：

- `runtime` 为空时默认走 `claude-code`
- `command` 是当前真正的执行入口

代码：

- `internal/config/types.go`
- `internal/config/loader.go`
- `internal/launcher/launcher.go`

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

## 当前工作流触发方式

当前真正生效的是 label 驱动模型：

- issue 带上 workflow trigger label 后，该 workflow 才参与匹配
- state machine 通过当前 issue labels 匹配 `enter_label` 推断当前状态
- 当某个状态 label 被新增，且该状态配置了 `agent`，就触发 dispatch

关键代码：

- `internal/statemachine/statemachine.go`
- `.github/workbuddy/workflows/feature-dev.md`
- `.github/workbuddy/workflows/bugfix.md`

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

- `dev-agent` / `test-agent` / `review-agent` 通过 `command` 把路由指令直接写进 prompt
- workflow 用 `when: labeled "status:..."` 表达状态切换
- 状态推进靠 agent 自己执行 `gh issue edit`
- Go 侧只观察结果，不替 agent 改 label

相关文件：

- `.github/workbuddy/agents/dev-agent.md`
- `.github/workbuddy/agents/test-agent.md`
- `.github/workbuddy/agents/review-agent.md`
- `.github/workbuddy/agents/codex-dev-agent.md`
- `.github/workbuddy/workflows/feature-dev.md`
- `.github/workbuddy/workflows/bugfix.md`
