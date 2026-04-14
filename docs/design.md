# Workbuddy — 需求设计文档

## 1. 项目概述

Workbuddy 是一个基于 GitHub 平台的 Agent 编排系统。它以 GitHub Issue 作为任务入口，通过 Label 驱动的状态机管理 Issue 生命周期，自动将任务路由到对应的 Worker 节点执行（启动 Claude Code 实例），并将结果写回 GitHub。

核心理念：**GitHub 即控制面**。Issue 是任务单，Label 是状态，Comment 是日志摘要。

### 1.1 设计哲学：Agent-as-Router (LangGraph 风格)

受 LangGraph 启发，workbuddy 中的每个 Agent 既是执行者也是路由器：

- **Agent = 图的节点**：每个 agent（dev-agent, test-agent, review-agent）对应状态图中的一个节点
- **Label = 图的边**：Agent 通过 `gh issue edit` 修改 label 来决定下一个状态
- **Coordinator = 图的运行时**：只负责检测 label 变化并派发任务，不参与路由决策

```
┌──────────┐   label:testing   ┌──────────┐   label:reviewing   ┌──────────┐
│dev-agent │ ───────────────► │test-agent│ ────────────────► │review-   │
│(node)    │                  │(node)    │                    │agent     │
│          │ ◄─────────────── │          │ ◄──────────────── │(node)    │
└──────────┘  label:developing └──────────┘  label:developing  └──────────┘
     执行完毕 → 自行决定                执行完毕 → 自行决定
     走哪条边 (改 label)              走哪条边 (改 label)
```

**关键区别**：传统 CI/CD 中，中心调度器决定"下一步做什么"。在 workbuddy 中，Agent 自己决定——Coordinator 只是忠实地检测 label 变化并触发对应的 Agent。人类也可以随时手动改 label 来干预流程。

### 1.2 设计目标

- 轻量级、自托管的 GitHub Agentic Workflows 替代方案
- 单个 Go 二进制文件，同时承载 Coordinator 和 Worker 角色
- **Hub-Spoke 架构**：Coordinator（公网）负责调度，Worker（各机器）负责执行
- 多仓库支持：Coordinator 管理多个 GitHub 仓库的 Issue 生命周期
- **仓库级隔离**：每个 repo 的 agent 绑定只在该 repo 范围内生效
- 配置格式参考 GitHub Agentic Workflows：Markdown + YAML frontmatter
- 所有 GitHub 交互通过 `gh` CLI 完成
- 持久化使用 SQLite（单文件数据库，无需额外服务）

### 1.2 典型使用场景

```
前置条件：
  - 公网机器运行 workbuddy coordinator，管理 repo Lincyaw/myproject
  - 开发机 A 运行 workbuddy worker，注册 repo=Lincyaw/myproject, roles=[dev]
  - CI 机器 B 运行 workbuddy worker，注册 repo=Lincyaw/myproject, roles=[test, review]
```

1. 开发者在 `Lincyaw/myproject` 创建 GitHub Issue，打上 `type:feature` 标签
2. Coordinator 轮询检测到新 Issue，状态机将其推进到 `developing`
3. Coordinator 查找注册了 `repo=Lincyaw/myproject, role=dev` 的 Worker
4. 开发机 A 通过长轮询领取任务，在 `Lincyaw/myproject` 仓库目录下启动 Claude Code 实例
5. Claude Code 创建分支、实现功能、开 PR
6. Coordinator 检测到 PR 创建，状态机推进到 `testing`
7. CI 机器 B 领取测试任务，启动 Claude Code 执行测试
8. 测试通过 → Coordinator 推进到 `reviewing` → 机器 B 领取 review 任务
9. Review 发现问题 → 状态回退到 `developing`（第 1 次重试）→ 机器 A 重新实现
10. 如果来回超过 3 次（可配置）→ 标记为 `status:failed`，需人工介入
11. Review 通过 → Issue 关闭，整个流程完成

### 1.3 多仓库场景

```
Coordinator 同时管理：
  - Lincyaw/frontend    (Worker C: roles=[dev, test])
  - Lincyaw/backend     (Worker A: roles=[dev], Worker B: roles=[test, review])
  - Lincyaw/infra       (Worker D: roles=[dev, deploy])

每个仓库有自己的 .github/workbuddy/ 配置。
Worker 注册时声明 repo + roles，互不干扰。
```

## 2. 系统架构

### 2.1 Hub-Spoke 总览

```
┌─────────────────────────────────────────────────────────────────┐
│                       GitHub (Control Plane)                      │
│  Issues = 任务单      Labels = 状态机      Comments = 日志摘要     │
└──────────────────────────────┬───────────────────────────────────┘
                               │
              ┌────────────────▼────────────────┐
              │      Coordinator (公网部署)        │
              │                                  │
              │  GitHub Poller ← gh CLI          │
              │  State Machine (cycle detection) │
              │  Task Router (repo + role)       │
              │  Worker Registry                 │
              │  Reporter → gh CLI               │
              │  SQLite (持久化)                  │
              │  HTTP API (Worker 通信 + 审计)    │
              └──┬──────────┬──────────┬─────────┘
                 │          │          │
           long poll    long poll    long poll
                 │          │          │
           ┌─────▼───┐ ┌───▼─────┐ ┌──▼──────┐
           │Worker A  │ │Worker B │ │Worker C │
           │          │ │         │ │         │
           │repo: X   │ │repo: X  │ │repo: Y  │
           │roles:    │ │roles:   │ │roles:   │
           │ - dev    │ │ - test  │ │ - dev   │
           │          │ │ - review│ │ - test  │
           │启动 Claude│ │启动 Claude││启动 Claude│
           │Code 实例 │ │Code 实例│ │Code 实例│
           └──────────┘ └─────────┘ └─────────┘
```

### 2.2 模块分层

```
┌────────────────────────────────────────────────────────┐
│                Layer 5: Workflow 编排                    │
│  Workflow Engine / Parallel Dispatch / Dependency Graph  │
├────────────────────────────────────────────────────────┤
│                Layer 4: 可观测性                         │
│  Event Log / Audit Server / Dashboard API               │
├────────────────────────────────────────────────────────┤
│                Layer 3: 通信层                           │
│  Coordinator HTTP API / Worker Client / Long Polling     │
├────────────────────────────────────────────────────────┤
│                Layer 2: Agent Runtime                    │
│  Claude Code Launcher / Session Manager                  │
├────────────────────────────────────────────────────────┤
│                Layer 1: Core 核心引擎                    │
│  Config Loader / GitHub Poller / State Machine /         │
│  Task Router / Reporter / Worker Registry / SQLite Store │
└────────────────────────────────────────────────────────┘
```

### 2.3 v0.1.0 单机合体模式

v0.1.0 中 Coordinator 和 Worker 运行在同一个进程内，通过 Go channel 直接通信（而非 HTTP）：

```
┌─────────────────────────────────────────┐
│  workbuddy serve (单进程)                │
│                                         │
│  ┌─── Coordinator 模块 ───┐             │
│  │  Poller → StateMachine  │             │
│  │  → TaskRouter ──────────┼──channel──┐ │
│  │  ← Reporter ◄───────── │           │ │
│  │  SQLite Store           │           │ │
│  └─────────────────────────┘           │ │
│                                        │ │
│  ┌─── Worker 模块 ─────────┐          │ │
│  │  TaskExecutor ◄─────────┼──channel──┘ │
│  │  → Claude Code Launcher │             │
│  │  → Result channel ──────┼─────────────┘
│  └─────────────────────────┘             │
└──────────────────────────────────────────┘
```

v0.2.0 将 channel 替换为 HTTP 长轮询，实现网络分离。**内部接口保持一致**，只替换传输层。

## 3. 模块详细设计

### 3.1 Layer 1: Core 核心引擎

#### 3.1.1 Config Loader（REQ-001）

**职责**：解析 `.github/workbuddy/` 下的配置文件。

**配置文件类型**：

| 文件 | 格式 | 位置 | 说明 |
|------|------|------|------|
| Agent 定义 | Markdown + YAML frontmatter | `.github/workbuddy/agents/*.md` | 定义 agent 触发条件、执行角色、超时 |
| Workflow 定义 | Markdown + YAML frontmatter | `.github/workbuddy/workflows/*.md` | 定义状态机、转换规则、重试上限 |
| 环境配置 | YAML | `.github/workbuddy/config.yaml` | 仓库名、轮询间隔、服务端口 |

**Agent 定义格式**：

```markdown
---
name: dev-agent
description: Development agent - implements features and fixes bugs
triggers:
  - label: "status:developing"    # 当 Issue 进入此状态时触发
    event: labeled
role: dev                          # Worker 角色名，用于路由到正确的机器
command: >                         # Claude Code 启动命令（含路由指令）
  claude -p "Implement the feature...
  When done, run: gh issue edit {{.Issue.Number}} --repo {{.Repo}}
    --remove-label status:developing --add-label status:testing"
timeout: 30m
---

## Dev Agent

（Markdown 正文作为 agent 说明文档。正文中应记录 Agent 的路由表：
什么结果 → 改什么 label → 进入什么状态）
```

**关键设计**：
- `triggers` 的 label 与 workflow 的 `enter_label` 对应——进入某状态就触发对应 Agent
- `role` 字段用于路由到注册了该 role 的 Worker 机器
- `command` 中的 Claude Code prompt **必须包含路由指令**：Agent 根据执行结果自行通过 `gh issue edit` 修改 label
- Markdown 正文中记录路由表（哪种结果 → 改哪个 label），作为可读文档

**Workflow 定义格式**：

```markdown
---
name: feature-dev
description: Full feature development lifecycle
trigger:
  issue_label: "type:feature"
max_retries: 3             # 新增：状态机环的最大重试次数（默认 3）
---

## States

```yaml
states:
  triage:
    enter_label: "status:triage"
    transitions:
      - to: developing
        when: labeled "agent:dev"
        assign: dev-agent
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: testing
        when: pr_opened
        assign: test-agent
  testing:
    enter_label: "status:testing"
    agent: test-agent
    transitions:
      - to: reviewing
        when: checks_passed
        assign: review-agent
      - to: developing                    # 环：回到 developing
        when: checks_failed
        assign: dev-agent
  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      - to: done
        when: approved
      - to: developing                    # 环：回到 developing
        when: changes_requested
        assign: dev-agent
  done:
    enter_label: "status:done"
    action: close_issue
  failed:                                  # 新增：超过重试上限进入此状态
    enter_label: "status:failed"
    action: add_label "needs-human"
```
```

**设计要点**：
- Frontmatter 解析：按 `---` 分割 + `yaml.Unmarshal`（无需 goldmark）
- Workflow 的 states 从 Markdown 正文的 ```yaml 代码块提取
- 启动时校验所有配置，报告错误
- `max_retries` 为 workflow 级别设置，对该 workflow 下所有回退边生效
- Workflow 的 states 从 Markdown 正文中**第一个** ```yaml 代码块提取；如果正文中没有 yaml 代码块则报错
- 配置加载后不支持热更新——修改 agent/workflow 配置文件后需重启 `workbuddy serve`（v0.1.0 限制）

#### 3.1.2 GitHub Poller（REQ-002）

**职责**：Coordinator 定时轮询 GitHub，检测 Issue/PR 状态变化。

**轮询策略**：

```
每个 poll cycle（per repo）:
  1. gh issue list --repo <repo> --state open --json number,title,labels,assignees,createdAt,updatedAt
  2. 与 SQLite 缓存对比，识别变化：
     - 新 Issue 创建
     - Label 增加/移除
     - Assignee 变更
     - Issue 关闭/重开
  3. gh pr list --repo <repo> --state open --json number,headRefName,state,reviews,statusCheckRollup
  4. 对比 PR 状态变化：
     - 新 PR 创建
     - Check 通过/失败
     - Review approved/changes_requested
  5. 将变化事件推送到 State Machine
```

**缓存**：
- 存储在 SQLite `issue_cache` 表中（重启后不丢失）
- 每次 poll 后更新

**崩溃恢复**：
- Coordinator 重启后，首次 poll 必须做**全量 diff**（将 GitHub 当前状态与 SQLite 缓存对比），而非仅看增量变化
- 原因：重启期间 Agent 可能已经修改了 label，但 Coordinator 没有收到事件
- 首次 poll 完成后恢复为增量检测模式

**多仓库轮询**：
- Coordinator 注册了 N 个仓库，每个仓库独立的 poll 循环
- 每个 repo 有自己的 `poll_interval`

#### 3.1.3 State Machine（REQ-003）

**职责**：被动检测 label 变化，维护状态一致性，追踪重试次数。

**核心原则**：State Machine 不做路由决策——它只观察 label 变化（无论是 Agent 还是人类修改的），验证转换合法性，并在回退时检查重试上限。

**状态机（含环与重试上限）**：

```
                  ┌──────────────┐
                  │   created    │
                  └──────┬───────┘
                         │ match workflow trigger label
                  ┌──────▼───────┐
                  │   triage     │
                  └──────┬───────┘
                         │ assign agent label
                  ┌──────▼───────┐
            ┌────►│  developing  │◄──── changes_requested (retry N/3)
            │     └──────┬───────┘
            │            │ PR opened
            │     ┌──────▼───────┐
            │     │   testing    │
            │     └──────┬───────┘
            │            │ checks passed
   checks   │     ┌──────▼───────┐
   failed   └─────│  reviewing   │
  (retry N/3)     └──────┬───────┘
                         │ approved
                  ┌──────▼───────┐
                  │    done      │ → close issue
                  └──────────────┘

   如果 retry count ≥ max_retries:
                  ┌──────────────┐
                  │   failed     │ → add label "needs-human"
                  └──────────────┘
```

**重试计数机制**：

```go
// SQLite 表: transition_counts
// issue_number | workflow | from_state | to_state | count

type CycleTracker struct {
    db *sql.DB
}

func (ct *CycleTracker) RecordAndCheck(issueNum int, workflow string, from, to string, maxRetries int) (allowed bool, count int) {
    // 1. 查询当前 issue 在此 workflow 中 from→to 的已有次数
    // 2. 如果 count < maxRetries → 允许转换，count++
    // 3. 如果 count >= maxRetries → 拒绝转换，改为转到 "failed" 状态
    // 返回是否允许，以及当前次数
}
```

**转换条件（Transition Conditions）**：

主要转换方式是 **Agent 修改 label**（Agent-as-Router），但也支持外部事件作为补充：

| 条件 | 触发源 | 说明 |
|------|--------|------|
| `labeled "<label>"` | Agent 通过 `gh issue edit` 修改，或人类手动修改 | **主要路由方式**——Agent 执行完毕后自行决定下一个状态 |
| `pr_opened` | Poller 检测到新 PR | 补充条件：CI 相关的外部事件 |
| `checks_passed` | Poller 检测 PR status checks | 补充条件：CI check 通过 |
| `checks_failed` | Poller 检测 PR status checks | 补充条件：CI check 失败 |
| `approved` | Poller 检测 PR review 状态 | 补充条件：人工 review |
| `changes_requested` | Poller 检测 PR review 状态 | 补充条件：人工 review |
| `comment_command "<cmd>"` | Poller 检测新 comment | 补充条件：`/approve` 等人工命令 |

**单边约束**：对于同一对起点状态和终点状态，一个 workflow 中只允许存在**一条 edge**（transition）。如果定义了多条从 A 到 B 的 transition，Config Loader 应在校验阶段报错。这避免了条件匹配时的歧义。

**典型流程**：Agent（Claude Code）执行任务 → 根据结果执行 `gh issue edit --add-label status:testing` → Poller 下一轮检测到 label 变化 → State Machine 匹配 `when: labeled "status:testing"` → 派发下一个 Agent。

**状态转换执行**：

```go
type Transition struct {
    To        string   // 目标状态
    When      string   // 条件表达式
    Assign    string   // 分配的 agent（可选）
}

type State struct {
    EnterLabel  string       // 进入时添加的 label
    Agent       string       // 当前状态绑定的 agent
    Transitions []Transition
}
```

转换时：
1. 检查 CycleTracker：如果是回退边且已超过 max_retries → 转到 `failed` 状态
2. 移除旧状态 label（`status:developing` → remove）
3. 添加新状态 label（`status:testing` → add）
4. 通知 Task Router 分配 agent（如果 transition 指定了 assign）
5. 记录转换事件到 Event Log

#### 3.1.4 Worker Registry（REQ-023 新增）

**职责**：管理已注册的 Worker 节点。

```go
type WorkerRegistration struct {
    WorkerID    string    // 唯一标识（自动生成或 Worker 指定）
    Repo        string    // owner/repo
    Roles       []string  // ["dev", "test", "review"]
    Hostname    string    // Worker 机器名（信息性）
    RegisteredAt time.Time
    LastHeartbeat time.Time
    Status      string   // online | offline
}
```

**存储**：SQLite `workers` 表

**行为**：
- Worker 注册：INSERT or UPDATE（同一 WorkerID 重新注册时更新）
- 心跳：Worker 每次长轮询都算一次心跳，更新 `LastHeartbeat`
- 离线检测：`LastHeartbeat` 超过 3 个 poll 周期 → 标记为 offline
- 查询：按 repo + role 查找可用 Worker

#### 3.1.5 Task Router（REQ-004 重新设计）

**职责**：根据状态机转换结果，将任务路由到匹配的 Worker。替代原来的 Dispatcher。

**路由流程**：

```
1. 收到 State Machine 的派发请求（repo, issue_number, agent_name）
2. 从 Config 查找 agent 定义 → 获取 role 字段
3. 在 Worker Registry 中查找：repo 匹配 + role 匹配 + status=online
   - 无匹配 Worker → 任务进入等待队列，等下次 Worker 长轮询时派发
   - 有匹配 Worker → 将任务放入该 Worker 的任务队列
   - 多个匹配 Worker → 选择最近心跳的那个（简单策略，不做负载均衡）
4. 任务包含完整上下文：
   - Issue title, body, labels, comments
   - 关联 PR 信息（如有）
   - Agent 配置（command, timeout）
   - Session ID
```

**任务队列**：SQLite `task_queue` 表

```sql
CREATE TABLE task_queue (
    id          TEXT PRIMARY KEY,
    repo        TEXT NOT NULL,
    issue_num   INTEGER NOT NULL,
    agent_name  TEXT NOT NULL,
    role        TEXT NOT NULL,
    context     TEXT NOT NULL,   -- JSON: issue info, agent config
    status      TEXT NOT NULL,   -- pending | assigned | running | completed | failed
    worker_id   TEXT,            -- 被分配的 Worker
    created_at  DATETIME,
    assigned_at DATETIME,
    completed_at DATETIME,
    result      TEXT             -- JSON: exit_code, stdout, stderr, duration
);
```

#### 3.1.6 Reporter（REQ-005 / REQ-010）

**职责**：Coordinator 将 Agent 执行结果写回 GitHub。

**报告内容**：

```markdown
## Agent: dev-agent

**Status**: Completed / Failed / Timeout
**Duration**: 4m 32s
**Session**: `sess-20260414-abc123`
**Worker**: worker-dev-01 (machine-a)
**Retry**: 1/3

### Summary
Implemented user registration endpoint.
Created branch `feat/42-user-registration` and opened PR #43.

### Output
<details>
<summary>Agent output (click to expand)</summary>

... (truncated agent stdout/stderr) ...

</details>

---
_Reported by workbuddy coordinator at 2026-04-14 09:30:00 UTC_
```

新增字段：Worker 信息、Retry 计数。

**操作**：
- `gh issue comment <number> --body "<report>"`
- `gh issue edit <number> --add-label "status:<next>" --remove-label "status:<prev>"`

#### 3.1.7 SQLite Store（REQ-024 新增）

**职责**：Coordinator 的持久化层。

**表结构**：

| 表名 | 用途 | 主要字段 |
|------|------|---------|
| `repos` | 已注册的仓库 | repo, poll_interval, config_path, registered_at |
| `workers` | Worker 注册表 | worker_id, repo, roles (JSON), hostname, last_heartbeat, status |
| `issue_cache` | Issue/PR 状态缓存 | repo, issue_num, labels (JSON), pr_state, last_seen |
| `task_queue` | 任务队列 | id, repo, issue_num, agent_name, role, context, status, worker_id |
| `transition_counts` | 状态机重试计数 | repo, issue_num, workflow, from_state, to_state, count |
| `events` | 事件日志 | id, ts, type, repo, issue_num, payload |
| `sessions` | Agent 执行会话 | id, repo, issue_num, agent_name, worker_id, status, result |

**数据库文件位置**：`.workbuddy/workbuddy.db`

**设计要点**：
- 使用 `modernc.org/sqlite`（纯 Go，无 CGO 依赖）或 `mattn/go-sqlite3`
- 启动时自动建表（`CREATE TABLE IF NOT EXISTS`）
- 使用 WAL 模式提高并发读写性能

### 3.2 Layer 2: Agent Runtime

#### 3.2.1 Claude Code Launcher（REQ-006 重新设计）

**职责**：Worker 端根据 agent 配置启动 Claude Code 实例。

```go
type ClaudeCodeLauncher struct{}

func (l *ClaudeCodeLauncher) Launch(ctx context.Context, task *Task) (*Result, error) {
    // 1. 切换到 task.Repo 对应的本地仓库目录
    // 2. 对 agent command 进行 Go template 渲染
    // 3. 执行命令（启动 Claude Code 子进程）
    // 4. 设置环境变量：
    //    WORKBUDDY_ISSUE_NUMBER, WORKBUDDY_ISSUE_TITLE,
    //    WORKBUDDY_ISSUE_BODY, WORKBUDDY_REPO, WORKBUDDY_SESSION_ID
    // 5. 捕获 stdout/stderr
    // 6. 超时控制（agent 配置的 timeout）
    // 7. 返回 Result{ExitCode, Stdout, Stderr, Duration}
}
```

**Worker 端的仓库目录映射**：

Worker 启动时配置每个 repo 在本地的路径：

```yaml
# worker-config.yaml
coordinator: http://coordinator.example.com:8080
worker_id: worker-dev-01
repos:
  - name: Lincyaw/myproject
    path: /home/user/projects/myproject    # 本地仓库路径
    roles: [dev]
  - name: Lincyaw/frontend
    path: /home/user/projects/frontend
    roles: [dev, test]
```

**模板变量**：

| 变量 | 说明 |
|------|------|
| `{{.Issue.Number}}` | Issue 编号 |
| `{{.Issue.Title}}` | Issue 标题 |
| `{{.Issue.Body}}` | Issue 正文 |
| `{{.Issue.Labels}}` | Label 列表 |
| `{{.PR.URL}}` | 关联 PR 的 URL |
| `{{.PR.Branch}}` | PR 分支名 |
| `{{.Repo}}` | 仓库名 owner/repo |
| `{{.Session.ID}}` | 会话 ID |

**Agent 结构化输出协议**：

Agent（Claude Code）的 stdout 可能包含结构化元数据块，Launcher 解析后提取供 Reporter 使用。格式为 stdout 末尾的 JSON 块，用特殊标记包裹：

```
--- WORKBUDDY_META ---
{"pr_url": "https://github.com/owner/repo/pull/43", "branch": "feat/42-user-reg", "commit_sha": "abc1234", "summary": "Implemented user registration endpoint"}
--- END_WORKBUDDY_META ---
```

解析规则：
- Launcher 扫描 stdout 中最后一个 `--- WORKBUDDY_META ---` 和 `--- END_WORKBUDDY_META ---` 之间的内容
- 如果不存在或解析失败，Result.Meta 为空（不报错）——向后兼容不输出 meta 的 agent
- 所有字段可选：`pr_url`, `branch`, `commit_sha`, `summary`, `files_changed`
- Reporter 优先使用 Meta 中的信息（如 PR URL），比正则提取 stdout 更可靠

#### 3.2.2 Session Manager（REQ-008）

**职责**：管理 Agent 执行会话。

```go
type Session struct {
    ID        string
    Repo      string
    AgentName string
    IssueNum  int
    WorkerID  string
    Status    SessionStatus  // pending | running | completed | failed | cancelled | timeout
    RetryNum  int            // 第几次重试
    StartedAt time.Time
    EndedAt   time.Time
    Result    *Result
}
```

存储在 SQLite `sessions` 表中。

功能：
- 创建/查询/取消 session
- 防止同一 Issue 同时运行多个同类 agent
- 按 repo/worker/agent/status 查询

### 3.3 Layer 3: 通信层

#### 3.3.1 Coordinator HTTP API（REQ-025 新增）

**职责**：Coordinator 对外暴露 HTTP API，供 Worker 长轮询和管理操作。

**端点设计**：

| Method | Path | 说明 | 版本 |
|--------|------|------|------|
| POST | `/api/v1/workers/register` | Worker 注册（repo + roles） | v0.1.0 |
| GET | `/api/v1/tasks/poll` | Worker 长轮询获取任务 | v0.1.0 |
| POST | `/api/v1/tasks/:id/result` | Worker 提交执行结果 | v0.1.0 |
| POST | `/api/v1/tasks/:id/heartbeat` | Worker 报告任务执行中（心跳） | v0.1.0 |
| GET | `/api/v1/workers` | 查询已注册 Worker 列表 | v0.2.0 |
| GET | `/api/v1/repos` | 查询已注册仓库列表 | v0.2.0 |
| POST | `/api/v1/repos/register` | 注册新仓库 | v0.2.0 |
| GET | `/api/v1/status` | Coordinator 状态概览 | v0.2.0 |
| GET | `/api/v1/sessions` | Session 列表 | v0.2.0 |
| GET | `/api/v1/events` | 事件查询 | v0.2.0 |
| GET | `/health` | 健康检查 | v0.1.0 |

**长轮询协议**：

```
Worker → GET /api/v1/tasks/poll?worker_id=xxx&timeout=30s

  如果有匹配任务:
    ← 200 OK + Task JSON（立即返回）

  如果没有匹配任务:
    Coordinator 挂起请求，等待最多 timeout 秒
    期间有新任务 → 立即返回
    超时 → 204 No Content（Worker 重新发起请求）
```

**v0.1.0 单机模式**：HTTP API 仍然启动（用于审计），但 Coordinator 和 Worker 之间通过 Go channel 直接通信，不走 HTTP。

#### 3.3.2 Worker Client（REQ-026 新增）

**职责**：Worker 端的 HTTP client，负责与 Coordinator 通信。

```go
type WorkerClient struct {
    coordinatorURL string
    workerID       string
    httpClient     *http.Client
}

func (c *WorkerClient) Register(ctx context.Context, repos []RepoRole) error
func (c *WorkerClient) PollTask(ctx context.Context, timeout time.Duration) (*Task, error)
func (c *WorkerClient) SubmitResult(ctx context.Context, taskID string, result *Result) error
func (c *WorkerClient) Heartbeat(ctx context.Context, taskID string) error
```

**v0.1.0 单机模式**：使用 `LocalWorkerClient` 替代，直接读写内存 channel。接口不变。

### 3.4 Layer 4: 可观测性

#### 3.4.1 Event Log（REQ-009）

**职责**：记录所有事件。

**v0.1.0**：写入 SQLite `events` 表（替代原来的 JSONL 文件）。

**事件类型**：

```json
{"ts":"...","type":"poll","repo":"Lincyaw/myproject","issues_checked":15,"changes_detected":2}
{"ts":"...","type":"transition","repo":"...","issue":42,"from":"triage","to":"developing","agent":"dev-agent"}
{"ts":"...","type":"dispatch","repo":"...","issue":42,"agent":"dev-agent","worker":"worker-dev-01","session":"sess-abc123"}
{"ts":"...","type":"completed","repo":"...","issue":42,"agent":"dev-agent","session":"sess-abc123","duration":"4m32s","exit_code":0}
{"ts":"...","type":"retry_limit","repo":"...","issue":42,"edge":"reviewing→developing","count":3,"max":3}
{"ts":"...","type":"report","repo":"...","issue":42,"comment_id":12345}
{"ts":"...","type":"worker_registered","worker":"worker-dev-01","repo":"...","roles":["dev"]}
{"ts":"...","type":"worker_offline","worker":"worker-dev-01"}
{"ts":"...","type":"error","repo":"...","issue":42,"agent":"dev-agent","error":"timeout after 30m"}
```

新增事件类型：`retry_limit`（重试上限触发）、`worker_registered`、`worker_offline`。

#### 3.4.2 Audit HTTP Server（REQ-011）

与原设计基本一致，但数据源改为 SQLite。

#### 3.4.3 Dashboard API（REQ-012）

与原设计一致，额外暴露 Worker 状态。

### 3.5 Layer 5: Workflow 编排

#### 3.5.1 Transition Rules Engine（REQ-013）

与原设计一致。

#### 3.5.2 Workflow Engine（REQ-014）

与原设计一致。

#### 3.5.3 Parallel Dispatch（REQ-015）

与原设计一致。

#### 3.5.4 Issue Dependency Graph（REQ-016）

与原设计一致。

## 4. CLI 设计

```
workbuddy <command> [flags]

Commands:
  serve         v0.1.0 单机模式：内嵌 Coordinator + Worker
  coordinator   v0.2.0 启动 Coordinator（公网部署）
  worker        v0.2.0 启动 Worker（连接到远程 Coordinator）
  init          初始化 .github/workbuddy/ 配置目录
  status        显示当前状态
  run           手动触发 agent 处理指定 issue
  validate      校验配置文件
  logs          查看执行日志

Global Flags:
  --repo        目标仓库（覆盖 config.yaml 中的设置）
  --config      配置文件路径（默认 .github/workbuddy/config.yaml）
  --verbose     详细输出
  --db          SQLite 数据库路径（默认 .workbuddy/workbuddy.db）
```

### 4.1 serve（REQ-017，v0.1.0）

```
workbuddy serve [flags]

Flags:
  --port int                 HTTP 服务端口（默认 8080）
  --poll-interval duration   轮询间隔（默认 30s）
  --roles strings            Worker 角色列表（默认从 config 读取）

行为：
  1. 加载配置（agent/workflow/config）
  2. 初始化 SQLite
  3. 启动 GitHub Poller（后台 goroutine）
  4. 启动 State Machine + Task Router
  5. 启动内嵌 Worker（通过 channel 接收任务）
  6. 启动 HTTP Server（API + 审计）
  7. 监听 SIGINT/SIGTERM，优雅关闭
     - 停止 Poller
     - 等待运行中的 Agent 完成（最多 60s）
     - 关闭 HTTP Server
```

### 4.2 coordinator（REQ-027 新增，v0.2.0）

```
workbuddy coordinator [flags]

Flags:
  --port int                 HTTP 服务端口（默认 8080）
  --poll-interval duration   轮询间隔（默认 30s）

行为：
  1. 加载配置
  2. 初始化 SQLite
  3. 启动 Poller + State Machine + Task Router
  4. 启动 HTTP Server（Worker 长轮询 API + 审计）
  5. 不启动 Worker——等待远程 Worker 注册和拉取任务
```

### 4.3 worker（REQ-028 新增，v0.2.0）

```
workbuddy worker [flags]

Flags:
  --coordinator string       Coordinator URL（如 http://coordinator.example.com:8080）
  --worker-config string     Worker 配置文件路径（默认 worker-config.yaml）
  --worker-id string         Worker ID（默认自动生成）

行为：
  1. 加载 worker-config.yaml（repo → 本地路径 + roles 映射）
  2. 向 Coordinator 注册
  3. 循环长轮询 Coordinator 获取任务
  4. 收到任务 → 启动 Claude Code 实例执行
  5. 执行期间定期发送心跳
  6. 执行完成 → 提交结果给 Coordinator
```

### 4.4 init（REQ-018）

与原设计一致。

### 4.5 status（REQ-019）

```
workbuddy status [flags]

Flags:
  --stuck          只显示卡住的 issue
  --workers        显示已注册 Worker 列表
  --json           JSON 输出

输出示例：
  Coordinator: running (port 8080)
  Database: .workbuddy/workbuddy.db (15.2 MB)
  Repos: 2 registered
  Last poll: 3s ago

  Workers:
    worker-dev-01   Lincyaw/myproject  [dev]       online   heartbeat 5s ago
    worker-ci-01    Lincyaw/myproject  [test,review] online   heartbeat 12s ago

  Active Sessions:
    sess-abc123  Lincyaw/myproject  #42  dev-agent     running  4m ago  worker-dev-01
    sess-def456  Lincyaw/myproject  #45  test-agent    running  1m ago  worker-ci-01

  Stuck Issues:
    (none)

  Retry Warnings:
    #42  reviewing→developing  2/3 retries used
```

### 4.6 run（REQ-020）

与原设计一致。

### 4.7 validate（REQ-021）

与原设计一致。

### 4.8 logs（REQ-022）

与原设计一致，但数据源改为 SQLite。

## 5. 数据流

### 5.1 v0.1.0 单机模式（Agent-as-Router 闭环）

```
人类/Agent 修改 Issue label
        │
        ▼
   GitHub Poller ──poll──► gh issue list / gh pr list
        │
        │ 检测到 label 变化
        ▼
   State Machine ──evaluate──► Transition Rules + CycleTracker
        │                              │
        │ retry < max                  │ retry >= max
        ▼                              ▼
   Task Router                     转到 failed 状态
        │                          → add label "needs-human"
        │ 按 repo + role 查找 Worker
        ▼
   Worker (内嵌, channel)
        │
        │ 启动 Claude Code 实例
        ▼
   Claude Code ──执行任务──► 实现/测试/审查
        │
        │ 执行完成，Agent 自行决定路由：
        │ gh issue edit --add-label status:<next>    ◄── Agent-as-Router
        │                                                (修改 label = 决定下一步)
        ▼
   Reporter ──write──► gh issue comment（执行摘要）
        │
        ▼
   SQLite ──record──► events / sessions / transition_counts
        │
        └──► 下一轮 Poller 检测到新 label → 循环继续
```

### 5.2 v0.2.0 分布式模式

```
GitHub Issue 创建/更新
        │
        ▼
   Coordinator: Poller ──poll──► gh issue list / gh pr list
        │
        ▼
   Coordinator: State Machine + CycleTracker
        │
        ▼
   Coordinator: Task Router ──lookup──► Worker Registry (SQLite)
        │
        │ 任务入队 (SQLite task_queue)
        ▼
   Coordinator: HTTP API ◄──long poll──► Worker Client
        │
        │ Worker 领取任务
        ▼
   Worker: Claude Code Launcher
        │
        │ 执行完成
        ▼
   Worker: HTTP POST /tasks/:id/result ──► Coordinator
        │
        ▼
   Coordinator: Reporter ──write──► gh issue comment
```

## 6. 版本计划

### v0.1.0 — 最小闭环·单机合体

**目标**：单机跑通一个 Issue 从创建到 Claude Code 执行到结果回写的完整流程，含环重试。

**验收标准**：
1. `workbuddy serve` 启动后，创建 Issue + label → 状态机检测 → Claude Code 执行 → 结果评论回写 → label 更新
2. 状态机回退（如 review → developing）正常工作，重试计数正确
3. 超过重试上限 → 自动标记 `status:failed` + `needs-human`
4. SQLite 数据库正确记录所有状态
5. `go build ./...` + `go test ./... -count=1` + `go vet ./...` 全部通过
6. E2E 集成测试通过：使用 mock GHExecutor 模拟完整闭环（创建 Issue → Agent mock 执行 → mock Agent 改 label → 状态机检测 → 下一个 Agent mock 触发 → 完成或重试上限）

**包含**：
- REQ-001: Config Loader
- REQ-002: GitHub Poller
- REQ-003: State Machine（含 CycleTracker）
- REQ-004: Task Router（单机版，channel 通信）
- REQ-005/010: Reporter
- REQ-006: Claude Code Launcher
- REQ-009: Event Log（SQLite）
- REQ-013: Transition Rules
- REQ-017: CLI serve
- REQ-023: Worker Registry（单机内嵌版）
- REQ-024: SQLite Store

### v0.2.0 — 分布式 + 可观测性

**目标**：Coordinator 和 Worker 分离，通过长轮询通信。多仓库支持。

**验收标准**：
1. `workbuddy coordinator` 和 `workbuddy worker` 可在不同机器运行
2. Worker 长轮询 Coordinator 获取任务并提交结果
3. 多仓库注册和隔离路由工作正常
4. Audit HTTP server 可浏览 sessions、events、workers

**包含**：
- REQ-025: Coordinator HTTP API（长轮询）
- REQ-026: Worker Client
- REQ-027: CLI coordinator
- REQ-028: CLI worker
- REQ-007: Remote Runner（可选，GitHub Actions 作为另一种 Worker 类型）
- REQ-008: Session Manager（完整生命周期 + SQLite 持久化）
- REQ-011: Audit HTTP Server
- REQ-018: CLI init
- REQ-019: CLI status（含 Worker 列表）
- REQ-020: CLI run
- REQ-021: CLI validate
- REQ-022: CLI logs

### v0.3.0 — 高级编排

**目标**：多步骤 workflow 编排、并行 agent 执行、Issue 依赖管理。

**包含**：
- REQ-012: Dashboard API
- REQ-014: Workflow Engine
- REQ-015: Parallel Dispatch
- REQ-016: Issue Dependency Graph

## 7. 目录结构（规划）

```
workbuddy/
├── cmd/
│   └── workbuddy/
│       └── main.go                # 入口
├── internal/
│   ├── config/
│   │   ├── loader.go              # Markdown + YAML frontmatter 解析
│   │   ├── types.go               # AgentConfig, WorkflowConfig, EnvConfig
│   │   └── loader_test.go
│   ├── store/
│   │   ├── sqlite.go              # SQLite 初始化、建表、通用操作
│   │   └── sqlite_test.go
│   ├── poller/
│   │   ├── poller.go              # GitHub 轮询逻辑
│   │   ├── gh.go                  # GHExecutor 接口 + 实现
│   │   └── poller_test.go
│   ├── statemachine/
│   │   ├── machine.go             # 状态机核心
│   │   ├── condition.go           # 转换条件求值
│   │   ├── cycle.go               # CycleTracker（重试计数）
│   │   └── machine_test.go
│   ├── router/
│   │   ├── router.go              # Task Router（替代 dispatcher）
│   │   └── router_test.go
│   ├── registry/
│   │   ├── registry.go            # Worker Registry
│   │   └── registry_test.go
│   ├── worker/
│   │   ├── executor.go            # TaskExecutor（Worker 端主循环）
│   │   ├── launcher.go            # Claude Code Launcher
│   │   ├── client.go              # Worker HTTP Client（v0.2.0）
│   │   └── executor_test.go
│   ├── reporter/
│   │   ├── reporter.go            # 结果回写 GitHub
│   │   ├── format.go              # Markdown 格式化
│   │   └── reporter_test.go
│   ├── session/
│   │   ├── manager.go             # 会话管理
│   │   └── manager_test.go
│   ├── eventlog/
│   │   ├── log.go                 # 事件日志（写入 SQLite）
│   │   └── log_test.go
│   ├── server/
│   │   ├── server.go              # HTTP Server（API + 审计）
│   │   ├── api.go                 # API 路由
│   │   └── server_test.go
│   └── app/
│       ├── serve.go               # v0.1.0 serve 模式：组装 Coordinator + Worker
│       ├── coordinator.go         # v0.2.0 纯 Coordinator 模式
│       └── worker.go              # v0.2.0 纯 Worker 模式
├── .github/
│   └── workbuddy/
│       ├── config.yaml
│       ├── agents/
│       │   ├── dev-agent.md
│       │   ├── test-agent.md
│       │   └── review-agent.md
│       └── workflows/
│           ├── feature-dev.md
│           └── bugfix.md
├── docs/
│   ├── design.md                  # 本文档
│   └── v0.1.0-roadmap.md          # 实施路线图
├── go.mod
├── go.sum
├── CLAUDE.md
├── project-index.yaml
└── .gitignore
```

## 8. 关键设计决策

### 8.1 为什么用 Hub-Spoke 架构而不是独立 Poller？

- 单点 Coordinator 避免多实例之间的状态同步和竞争
- Worker 可以在任何网络环境（内网、开发机），只要能访问 Coordinator
- Coordinator 集中管理所有仓库的状态机，逻辑清晰
- 仓库级隔离天然实现——Coordinator 按 repo + role 路由

### 8.2 为什么用 `gh` CLI 而不是 GitHub REST API？

- `gh` 已经处理好认证（`gh auth login`），无需管理 token
- 命令式调用更简单，减少 HTTP client 样板代码
- `gh` 的 JSON 输出模式（`--json`）足够结构化
- 用户在终端可以直接复用相同的命令调试

### 8.3 为什么用 Label 驱动状态机而不是 webhook？

- Webhook 需要公网暴露端点或 ngrok，增加部署复杂度
- Polling + Label 方案对 Worker 网络环境无要求
- Label 天然可视——在 GitHub UI 上直接看到 Issue 状态
- Coordinator 单点 poll 比多实例 poll 更可控

### 8.4 为什么用 SQLite 而不是 JSONL 或 PostgreSQL？

- JSONL 不支持高效查询，Worker 注册表和任务队列需要按条件检索
- PostgreSQL 需要额外部署数据库服务，不符合"单二进制"理念
- SQLite 是嵌入式单文件数据库，零部署成本
- WAL 模式支持并发读写，满足 Coordinator 的需求
- 纯 Go 实现（`modernc.org/sqlite`）无 CGO 依赖，交叉编译友好

### 8.5 为什么配置用 Markdown 而不是纯 YAML？

- 与 GitHub Agentic Workflows 保持一致
- Markdown 正文可以写详细的 agent 说明文档
- YAML frontmatter 提供结构化配置
- 在 GitHub 上直接渲染为可读文档

### 8.6 为什么状态机允许环并设重试上限？

- 现实中 review 不通过需要回到开发，测试失败需要修复——环是必要的
- 无限循环会导致 Issue 永远无法结束，浪费 Agent 算力
- Per-issue per-edge 的重试计数比全局计数更精确
- 超过上限后标记为 `failed` + `needs-human`，给人类一个明确的介入信号
- `max_retries` 可在 workflow 级别配置，不同 workflow 可以有不同的容忍度

### 8.7 为什么 v0.1.0 用单机合体而不是直接做分布式？

- 先验证核心逻辑正确性（状态机、Agent 执行、环重试），再加网络层
- 单机模式用 Go channel 替代 HTTP，减少调试复杂度
- 内部接口（TaskRouter interface、WorkerClient interface）保持一致，v0.2.0 只替换传输层实现
- 避免同时解决"业务逻辑"和"网络通信"两类问题

### 8.8 Stuck 检测的准确定义

Agent-as-Router 模式下，Agent 执行完毕时 label 就应该已经被 Agent 自己改了。真正的 stuck 场景是 **Agent 异常退出（崩溃、超时）但没有修改 label**。因此 stuck 检测的触发条件是：

- 任务状态变为 `completed`/`failed`/`timeout`（即 Agent 执行结束）
- 但对应 Issue 的 label 在任务结束后 **N 分钟内没有变化**（可配置，默认 5min）
- 此时标记 Issue 为 stuck，记录事件到 Event Log

这比纯粹的"超时未变"更准确——避免了 Agent 还在正常执行中就被误判为 stuck。

### 8.9 为什么让 Agent 自己改 label（Agent-as-Router）？

- **LangGraph 哲学**：每个 Agent 既是执行者也是路由器，执行完毕后自行决定下一步
- **简化 Coordinator**：Coordinator 只需检测 label 变化并派发 Agent，不需要理解执行结果
- **复用 GitHub 机制**：`gh issue edit --add-label` 是现成的 API，Agent（Claude Code）天然能调用
- **人类可干预**：任何人在 GitHub UI 上改 label 也能触发状态转换，不被自动化锁死
- **Agent 有上下文**：Agent 最了解自己执行结果，由它决定"测试通过了去 review"还是"还没准备好留在 developing"比中心调度器猜测退出码含义要准确
- **可观测性**：所有路由决策在 GitHub label 历史中有据可查
