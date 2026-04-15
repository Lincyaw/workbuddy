# Issue Dependency Mechanism

状态：implemented

## Status: minimal 2-agent design

> **The implementation diverged from the original draft below.** The shipped
> design is intentionally smaller:
>
> - **No `dependency-resolver-agent`.** The agent catalog stays at exactly
>   two agents (`dev-agent`, `review-agent`).
> - **No managed comment.** Dependency state is surfaced on GitHub via a
>   single 😕 **confused reaction** added to the issue when blocked and
>   removed when unblocked. This is the only GitHub UX signal beyond the
>   regular agent comment stream.
> - **No reconcile queue / generation table.** The Coordinator computes the
>   verdict in-process every poll cycle and only writes the
>   `issue_dependency_state` row plus the reaction.
> - **Dispatch gate.** Both `internal/router/router.go` and
>   `internal/statemachine/statemachine.go` look up the verdict and refuse
>   to dispatch when it is `blocked` or `needs_human`. The state machine
>   logs `dispatch_blocked_by_dependency` events for audit.
> - **Pure-programmatic work** (parsing, cycle detection, verdict
>   computation, reaction add/remove) lives in Coordinator Go. Reactions
>   are written by `internal/reporter/reporter.go` via `gh api .../reactions`
>   — this is the one new GH write boundary granted to Go code beyond
>   plain comments.
> - The `override:force-unblock` label still works as an in-band human
>   override; it sets the verdict to `override` (treated as ready) but no
>   longer drives any DB-side reconcile-queue logic.
>
> The historical draft below is preserved for context (Option A, Option B,
> the original reconcile flow, etc.) but should be read as design
> archaeology, not as a description of running code.

## Goal

为 workbuddy 增加一个可生产使用的 issue dependency 机制，让 issue B 可以声明“在 issue A 满足前不得启动”，并且这个约束同时满足：

- 人类在 GitHub issue UI 中可见
- poller / coordinator 可稳定解析
- blocked / unblocked 行为幂等
- 给 v0.2.x 跨仓库依赖留下兼容路径

本文只定义设计，不改当前实现。

## 当前状态

今天的 workbuddy 仍然是纯 label 驱动状态机：

- `internal/poller/poller.go` 周期性拉取 open issues / PRs，并把 label 变化转成事件。
- `internal/statemachine/statemachine.go` 根据 workflow 当前 state 和事件决定是否 dispatch agent。
- `internal/router/router.go` 在收到 dispatch request 后立即准备任务上下文并下发 worker。
- `internal/store/store.go` 当前只缓存 issue labels / task queue / transition counts，没有 dependency 图模型。
- `CLAUDE.md` 明确规定 Go 侧 GitHub 边界：Go 代码只读 GitHub，并且只允许通过 Reporter 写普通 issue comment；label 改动必须由 agent 子进程通过 `gh issue edit` 执行。

结果是：

- 只要 issue 带上 `status:developing`，dev agent 就会被 dispatch。
- 系统不知道“这个 issue 虽然在 developing，但应该等另一个 issue 先完成”。
- 也没有统一的 blocked 可见性、force override、cycle detection、或依赖图查询接口。
- 旧草案把 `status:blocked`、`needs-human` 和 dependency managed comment 都放到 Go 侧 reconcile，这和现有 GitHub write boundary 冲突。

## Convention Compliance

### 选择：Option A — 保持现有 GH write boundary

本设计选择 **Option A**：Go 侧负责读取 GitHub、解析 dependency graph、计算 blocked verdict，并把“想要呈现到 GitHub 的目标态”写入本地持久化队列；真正的 GitHub 写操作全部交给一个专用的 dependency-resolver agent 执行。

职责边界如下：

- **Go 侧（`cmd/serve.go`, `internal/poller/`, `internal/store/`, `internal/statemachine/`）**
  - 读取 issue body / labels / PR 状态
  - 解析 `workbuddy.depends_on` 声明
  - 构建依赖图并计算 `ready` / `blocked` / `override` / `needs-human`
  - 把 desired label/comment state 写入 SQLite 的 dependency tables 和 reconcile queue
  - 在 dispatch 前根据本地 dependency verdict 阻止不安全的 agent 派发
- **Agent 侧（未来的 `.github/workbuddy/agents/dependency-resolver-agent.md`）**
  - 读取 reconcile queue 中待处理的 issue
  - 使用 `gh issue edit` 加/去 `status:blocked`、恢复 `resume_label`、添加 `needs-human`
  - 使用 `gh` CLI upsert 带 marker 的 dependency-status managed comment
  - 只在 GitHub 已收敛到目标态后回写“已应用”状态

这满足 `CLAUDE.md` 的原因：

- Go 没有新增 label 写权限，仍然只做 GitHub read path。
- GitHub label 改动仍由 agent 子进程执行。
- dependency managed comment 也交给同一个 agent 负责，避免“Go 写 comment + agent 改 label”形成双写源、难以保证幂等。
- 现有 Reporter comment 仍然只负责 agent 运行报告，不承担 dependency gating 真值。

如果 reviewer 只读本节，结论应当是：**是，这个设计尊重 GH write boundary。**

## 目标状态

v0.1 目标是引入“同仓库 issue 依赖”最小闭环：

1. issue body 中声明依赖。
2. poller 每轮解析 dependency graph。
3. Go 侧在 state machine dispatch 之前计算 blocked / ready / invalid verdict。
4. Go 侧把 desired GitHub surface 写入本地 reconcile queue，而不是直接改 label / comment。
5. dependency-resolver agent 消费队列，把 issue 切到 `status:blocked`、恢复 `resume_label`、在需要时添加 `needs-human`，并维护一条带 marker 的 dependency comment。
6. dispatch gating 同时看 workflow label 和 dependency verdict，因此即使 GitHub label 尚未收敛，也不会错误派发 agent。
7. 若出现 cycle、非法引用、unsupported cross-repo、或无法判定的 closed state，则进入 `needs-human` escalation，但依然通过 agent 路径落地到 GitHub。

无依赖 issue 的行为必须与今天完全一致。

## Declaration

### 选择：issue body 中的专用 YAML code block

v0.1 采用 issue body 内的显式代码块作为声明源：

````markdown
## Dependencies

```yaml
workbuddy:
  depends_on:
    - "#26"
    - "Lincyaw/workbuddy#27"
```
````

解析规则：

- 只解析第一个同时满足 `yaml` fence 且包含 `workbuddy` 根键的代码块。
- 文档里写的 `workbuddy.depends_on` 指的是 YAML path：根键 `workbuddy` 下的 `depends_on` 子键，**不是字面量 dotted key**。
- `depends_on` 必须是字符串数组。
- 同仓库简写 `#26` 会被规范化成 `Lincyaw/workbuddy#26`。
- 显式全名 `owner/repo#123` 在 v0.1 允许写入，但若不是当前 repo，则标记为“declared but unsupported in v0.1”。
- 大小写敏感：键名必须是 `workbuddy` / `depends_on`，避免自然语言误判。
- 重复依赖去重后按规范化 key 排序保存。
- issue body 其他位置的 `Closes #N`、自然语言里的 `depends on #N`、PR body 中的 closing keywords，都不参与 dependency 声明解析。

### 为什么不是 label / comment / Projects / sub-issues

- label 无法表达多个依赖，也不适合携带结构化跨仓库 ID。
- comment 可追加但难做“当前真值”，编辑和删除语义不稳定。
- GitHub Projects 不是 repo 内代码审查流程的稳定真源，CLI / token 权限面更大。
- GitHub 原生 sub-issue / issue relation 未来可以复用，但当前 API / UI 稳定性和仓库兼容性不够，不适合作为 v0.1 真源。

因此：GitHub issue body 做 human-visible canonical source，SQLite 做 machine cache。

## Storage Model

### GitHub 侧

- Canonical declaration source：issue body 中的 YAML block
- Visible blocked state：`status:blocked`
- Human override：`override:force-unblock`
- Human escalation：`needs-human`
- Managed explanation：issue comment，带固定 marker：

```text
<!-- workbuddy:dependency-status -->
```

GitHub 上与 dependency gating 相关的所有 label/comment side effects 都由 dependency-resolver agent 统一执行。

### SQLite 侧

新增三类持久化数据：

1. `issue_dependencies`

- `repo`
- `issue_num`
- `depends_on_repo`
- `depends_on_issue_num`
- `source_hash`
- `status` (`active`, `unsupported_cross_repo`, `invalid`, `removed`)

2. `issue_dependency_state`

- `repo`
- `issue_num`
- `verdict` (`ready`, `blocked`, `override`, `needs_human`)
- `resume_label`
- `blocked_reason_hash`
- `override_active`
- `graph_version`
- `last_comment_hash`
- `last_comment_id`
- `last_evaluated_at`

3. `dependency_reconcile_queue`

- `repo`
- `issue_num`
- `generation`
- `desired_blocked`
- `desired_resume_label`
- `desired_needs_human`
- `desired_comment_body`
- `desired_comment_hash`
- `status` (`queued`, `applied`, `superseded`, `failed`)
- `requested_at`
- `applied_at`
- `last_error`

设计原则：

- issue body 是 declaration 真源。
- SQLite 是解析缓存、dispatch hard gate、comment 幂等锚点、以及 reconciliation queue。
- Go 只写本地 state，不直接写 GitHub。
- Agent 只消费 queue，不重新成为 dependency truth source。

## Satisfaction Semantics

依赖 A 对 B 来说“满足”定义为以下任一条件：

1. A 仍是 open issue，且当前 labels 包含 `status:done`
2. A 已关闭，并且是“通过 linked PR 关闭”

明确不算满足的情况：

- A 只有 `status:reviewing`
- A 进入 `status:failed`
- A 被人工直接 close，但没有 linked PR 证据
- A 不存在 / 无权限读取 / 跨仓库且 v0.1 不支持

resolver 对 closed issue 的判断，不能仅凭“issue 不在 open list 中”。Go 侧 reader 需要按需补充单 issue / linked PR 读取能力，以确认：

- issue 当前 state
- close 原因是否可归因为 linked PR

如果 closed 但无法确认是否由 linked PR 关闭，默认不解锁，并进入 `needs-human` escalation。

## Poller Contract

### 运行位置

dependency resolution 放在每轮 poll 的 issue/PR diff 之后、state machine dispatch 之前。

顺序：

1. poller 拉取 issues / PRs，更新 cache。
2. dependency parser 重新解析当前 open issues 的 dependency declarations。
3. resolver 构建图并计算 `ready` / `blocked` / `override` / `needs_human` verdict。
4. resolver 更新 `issue_dependencies` / `issue_dependency_state`，并在 desired GitHub surface 与当前快照不一致时写入 `dependency_reconcile_queue`。
5. state machine / router 在真正 dispatch 前读取 `issue_dependency_state`，对 blocked issue 直接拒绝派发。
6. dependency-resolver agent 以 schedule-driven repair loop 方式消费 queue，并把 GitHub labels/comment 收敛到目标态。

之所以优先选 schedule-driven agent，而不是 label-triggered agent，是因为 Go 侧不能为了“叫醒 agent”而主动加一个系统 label；那样会再次越过 GH write boundary。调度器或 maintenance loop 读取本地 queue 更符合当前边界约束。

### 为什么不把逻辑塞进现有 state machine

当前 `internal/statemachine/statemachine.go` 的输入是假设“labels 已经代表真实可执行状态”。dependency gating 需要的是一个 repo 级 graph 视角，并且 side effect 由异步 agent 来执行：

- 需要读取整个 repo 的 dependency graph，而不只是单 issue 当前 event
- 需要把“dispatch hard gate”和“GitHub surface repair”拆成两个阶段
- 需要处理 body edits、closed deps、cross-issue fan-out、以及 queue retry

因此更适合做成 poller 后、router 前的 resolver + agent reconcile 组合，而不是把 label 写回直接塞进 state machine。

## Dependency-Resolver Agent

未来新增一个专用 agent：

- 路径：`.github/workbuddy/agents/dependency-resolver-agent.md`
- 角色：只负责 dependency GitHub side effects，不参与业务代码开发
- 输入：repo、issue number、queue generation、desired label state、desired comment body/hash
- 输出：只修改 GitHub issue labels / managed comment，并把 queue generation 标记为已应用或失败

agent 的最小流程：

1. 读取 queue 中最新的 `queued` generation。
2. 再读一次 GitHub 当前 labels + dependency marker comment，防止用旧快照误操作。
3. 若 `override:force-unblock` 已存在，则绝不重新加 `status:blocked`。
4. 使用 `gh issue edit` 做 label 收敛：
   - blocked 时：移除 runnable `resume_label`，添加 `status:blocked`
   - unblocked 时：移除 `status:blocked`，恢复 `resume_label`
   - needs-human 时：添加 `needs-human`
5. Upsert `<!-- workbuddy:dependency-status -->` comment，若 hash 未变则跳过。
6. 只有在 GitHub 已经匹配 desired state 时，才把该 generation 标记为 `applied`。

这保证了“Go 负责决定要什么，agent 负责真正落地 GitHub 变更”。

## Blocked-State UX

### Label 规则

blocked issue 使用：

- 保留 workflow trigger label，如 `type:feature`
- 去掉待执行状态 label，例如 `status:developing`
- 加上 `status:blocked`

同时在 SQLite 记录 `resume_label=status:developing`。

重要的是：**这些 label 变化发生在 dependency-resolver agent，而不是 Go runtime。**

这样做的原因：

- `status:blocked` 在 GitHub UI 明确可见。
- 不会误触发 `status:developing` 对应 agent。
- unblock 时可以无损恢复到原目标状态。

### Comment 模板

managed comment 至少包含：

- 当前依赖列表
- 每个依赖的状态：`done`, `open`, `failed`, `closed-without-linked-pr`, `unsupported-cross-repo`
- 当前是否由 `override:force-unblock` 绕过
- cycle / invalid reference / needs-human 提示（若存在）
- queue generation 或最后一次 evaluate 时间，便于审计

同一个 issue 只维护一条 dependency-status comment。内容更新而不是重复发新评论。

## Auto-Unblock Strategy

### v0.1 选择：polling-driven 计算 + scheduled reconcile

v0.1 采用两阶段 unblock：

- **Go 侧**：每轮 poll 重算 graph，决定某 issue 现在已经 `ready`
- **Agent 侧**：消费 queue，把 GitHub surface 从 `status:blocked` 收敛回 `resume_label`

原因：

- 当前系统已经有稳定的 poller；dependency 计算天然适合 polling。
- GitHub write boundary 要求 label/comment write 走 agent，因此需要单独的 reconcile 阶段。
- queue generation 让重启恢复和 retry 更容易做成幂等。

代价：

- unblock 延迟最多一个 poll interval + 一个 reconcile interval。
- ready verdict 已经成立但 GitHub label 尚未恢复时，state machine 不会“抢跑” dispatch。

这是故意的：dispatch 必须等 GitHub surface 与本地 verdict 一致，避免隐藏状态导致的人类误判。

## Dispatch Gating

在 dispatch 前做一个硬门：

- 如果 `issue_dependency_state.verdict == blocked`，`dispatchAgent(...)` 必须拒绝派发，即使 issue 还没来得及被 agent 改成 `status:blocked`。
- 如果 `verdict == override`，则按 ready 处理，但 managed comment 必须说明这是人工绕过。
- 如果 `verdict == ready` 但 `resume_label` 还没恢复到 GitHub labels，state machine 不主动伪造进入事件；等待 dependency-resolver agent 完成 label 收敛后，再走现有 workflow path。

router 在真正写入 task queue / 下发 worker 前还要做一次最终复核，避免：

- 同 tick 内刚 ready 又被新的 graph 结果重新 block
- queue generation 尚未应用完成时出现 stale dispatch

## Manual Override

v0.1 的唯一 override 信号是保留 label：

- `override:force-unblock`

行为：

- 只对当前 issue 生效，不向下游传播。
- resolver 看到该 label 时，把 verdict 置为 `override`，并 queue 一次 comment/label reconcile：
  - 移除 `status:blocked`
  - 恢复 `resume_label`（若缺失）
  - 更新 comment，明确记录“force-unblocked by human override”
- 去掉该 label 后，下一轮 poll 恢复正常 dependency 计算。

对于 `needs-human`：

- dependency-resolver agent 只负责**添加** dependency 相关的 `needs-human`
- 清除 `needs-human` 仍由人工完成，避免自动系统擅自抹掉人工 escalation 信号

## Idempotency Model

必须同时保证以下操作可重复执行而无副作用：

- poller 重跑
- coordinator 重启
- dependency-resolver agent 重试
- issue body 被重复保存
- 依赖满足后多轮重复 unblock

具体规则：

1. declaration 解析结果规范化后写入 SQLite，只有 `source_hash` 变化才更新 dependency edge。
2. blocked verdict 只在 `issue_dependency_state` 中收敛，不依赖瞬时 event。
3. GitHub side effect 不是“立刻执行”，而是写成 `dependency_reconcile_queue` generation；同 issue 旧 generation 会被标记为 `superseded`。
4. managed comment 通过 marker + `last_comment_id` + `desired_comment_hash` 定位；hash 未变则 agent 不更新。
5. label reconcile 采用“目标态收敛”，不是“看见事件就追加动作”。
6. router 与 state machine 都读取同一个 dependency verdict，避免出现“一边已判 blocked，另一边仍派发”的双真值。

## Cycle Detection

### 算法

对当前 repo 的 open issues 构建有向图 `issue -> dependency`，每轮解析后执行 DFS with color marking；v0.1 不需要更复杂的算法。

输出：

- cycle 上的所有节点
- cycle path，例如 `#28 -> #26 -> #27 -> #28`

### 触发时机

- 理想情况：issue body 更新后的下一轮 poll 立即发现
- 最晚：在 issue 即将 dispatch 之前，依赖 verdict 仍会阻止派发

### 发现后动作

- Go 侧把相关 issue 的 verdict 置为 `needs_human`
- queue 一次 agent reconcile：
  - 添加 `needs-human`
  - 保持或切换到 `status:blocked`
  - 更新 dependency-status comment，写明 cycle path
- 不自动 force-break cycle

这样满足“最迟 dispatch 前必须发现”，并避免 silent deadlock。

## Race Conditions

### 两个 dependency 在同一 tick 同时满足

resolver 以“本轮全图快照”计算 ready 集合。只要所有 deps 在同一快照里都满足，就只生成一条新的 reconcile generation。

### dependency 在 agent 已运行时满足或失效

- 满足：不影响当前运行任务，最终由下一轮常规计算决定是否继续推进。
- 失效：不取消当前任务，但 issue 后续自动推进会被 dependency verdict 阻断，直到依赖再次满足。

### dep 被关闭后又重新打开

- reopened issue 在下轮 poll 中重新进入 open graph。
- 所有依赖它的 issue 重新计算并 queue 为 blocked，除非有 `override:force-unblock`。

### coordinator 多实例 / 重启

SQLite 中的 dependency state + reconcile queue generation + GitHub comment marker 让 reconcile 是幂等的；重复实例最多重复尝试同一目标态，不应产生多条 comment 或重复 dispatch。

## Failed / Retry Interaction

依赖 issue 进入 `status:failed` 时，默认视为“未满足”，因此：

- dependent issue 继续 blocked
- dependency-status comment 明确标记 upstream failed
- dependent issue 不自动传播为 failed
- 默认也不额外扇出 `needs-human`，避免一个失败上游把整条链路都打成 escalation 风暴
- 如需继续推进，由人类在 upstream 修复后完成，或在 downstream 手工加 `override:force-unblock`

原因是：

- failed 只是“当前自动流程停止”，不是“依赖逻辑完成”
- 自动把失败向下游传播会制造过度放大和难以恢复的状态雪崩

## Cross-Repo Extension Path

v0.1：

- 语法允许 `owner/repo#123`
- parser 识别并存储 fully-qualified issue key
- 但 resolver 默认标记为 `unsupported-cross-repo`
- issue 进入 blocked，并通过 agent 路径写 comment；若人工必须介入，则同样通过 agent 加 `needs-human`

v0.2.x 扩展点：

- GH reader 增加按 repo 批量查询 issue / PR 的能力
- store 主键统一使用 `(repo, issue_num)`
- reconcile queue 支持按 repo shard
- observability / API 输出统一返回 fully-qualified issue key
- cycle detection 从单 repo 图扩展到多 repo 图

这样 v0.1 不会把语法设计死，但也不会误报“已经支持跨仓库”。

## Observability

至少提供两种查询入口：

1. CLI

- `workbuddy deps list --repo <owner/repo>`
- `workbuddy deps show --repo <owner/repo> --issue 28`

2. HTTP

- `GET /api/dependencies`
- `GET /api/dependencies/:repo/:issue`

返回字段至少包含：

- issue
- dependencies
- dependents
- verdict
- blocked_reasons
- resume_label
- override_active
- needs_human
- graph_version
- queue_generation
- last_evaluated_at
- last_applied_at

数据源优先来自 SQLite 的 dependency tables，而不是临时重新爬 GitHub。

## Concrete Code Touch Points

这部分只列将来实现会改哪里，不代表本 issue 要改代码。

### `internal/poller/` + `cmd/serve.go`

- `internal/poller/poller.go`
  需要在 issue diff 后把 issue body 解析结果交给 dependency resolver，而不是只看 labels。
- `cmd/serve.go`
  需要把 dependency resolver 接到 poll loop 与 state machine 之间，并提供按 issue / PR 精查的 read path。

### `internal/store/`

- `internal/store/store.go`
  需要新增 dependency graph、dependency state、reconcile queue、managed comment anchor 等表和 CRUD。
- `internal/store/types.go`
  需要新增 dependency edge / verdict / reconcile request 的 typed models。

### `internal/statemachine/`

- `internal/statemachine/statemachine.go`
  需要在 dispatch 前接受 dependency verdict，并在最终 dispatch 前做一次防竞态复核。
- 现有 `DispatchRequest` 可能需要携带 dependency snapshot version 或 queue generation。

### `internal/router/`

- `internal/router/router.go`
  需要在真正写入 task queue / 下发 worker 前再次检查 blocked verdict，避免 stale ready verdict。

### Agent / config surface

- `.github/workbuddy/agents/dependency-resolver-agent.md`
  需要新增专用 agent 定义，明确它只负责 dependency GitHub side effects。
- `internal/config/types.go` 与相应 workflow/config 文档
  后续若要支持 scheduler-based agent invocation，需要补充对应 trigger/config surface。

### 可视化 / 调试

- `internal/webui/handler.go`
  后续可把 blocked graph、queue generation、reason 暴露到 UI / API。

## Rejected Alternatives

### 方案 A：修改 `CLAUDE.md`，给 Go 一个 dependency label 写权限例外

也就是 issue #34 允许的 Option B。

放弃原因：

- 这会把“读取 graph + 决定 blocked + 直接改 GitHub label”重新耦合回 Go 进程。
- 一旦为 `status:blocked` 开例外，后续很容易继续为 `needs-human`、managed comment、resume label 恢复再开第二个例外。
- queue + agent 模式已经能提供 audit trail、dry-run 和 idempotency，没有必要扩大 Go 的 GitHub write surface。

### 方案 B：只靠 label 表达依赖

例如 `blocked-by:#26`、`blocked-by:#27`。

放弃原因：

- GitHub label 名过长且数量不稳定
- 多依赖、跨仓库和 cycle 定位都很难表达
- 解析和人类编辑都脆弱

### 方案 C：只靠 comment directive

例如人工评论 `/depends-on #26 #27`。

放弃原因：

- comment 流是 append-only，难定义“当前真值”
- 删除 / 编辑 / 多条指令冲突时语义复杂
- poller 当前也不以 comment 为主索引

### 方案 D：直接用 GitHub native sub-issues / Projects relation

放弃原因：

- 当前 workbuddy 主要依赖 repo 级 issue + `gh` CLI；引入 Projects 会扩大权限和运维面
- API / CLI 兼容性和脚本稳定性不如 issue body 中的显式 YAML
- 不利于离线审查和 code-review 中直接看到 dependency contract

## Distance From Current Code

这仍然是一项中等偏大的协调层改造，但它和旧草案相比，最大的差异是不再引入 Go-side GitHub writer。

距离主要在四处：

1. 现在系统按单 issue event 驱动，dependency 需要 repo 级 graph 视角。
2. 现在 store 没有 dependency 持久化模型，也没有 reconcile queue。
3. 现在 agent 都是 label/event 驱动，没有专门消费 maintenance queue 的 dependency-resolver agent。
4. 现在 dispatch 只受 workflow state 驱动，没有“本地 dependency verdict 先于 GitHub label 收敛”的硬门。

但它仍然是增量式改造，因为：

- 无依赖 issue 完全走现有路径
- workflow schema 可以基本保持不变
- 核心变化是新增 resolver + queue + agent，而不是改写整个 Router / Reporter / runtime

## Migration from Prior Draft

相对于 PR #30 合入的旧草案，这一版的关键变化是：

1. **去掉 Go-side label reconcile**：Go 不再直接加/去 `status:blocked`，也不恢复 `resume_label`。
2. **去掉 Go-side dependency managed comment**：dependency-status comment 与 dependency labels 改为同一个 agent 统一维护。
3. **新增 reconcile queue**：Go 只把 desired GitHub surface 写入 SQLite generation queue，agent 再去真正执行。
4. **dispatch 改看本地 verdict**：state machine / router 不再假设 GitHub label 已经先收敛，而是先看 dependency verdict。
5. **澄清 YAML 语义**：`workbuddy.depends_on` 明确是 YAML path，不是字面量 dotted key。
6. **收紧 escalation 语义**：`needs-human` 只用于 cycle、invalid reference、unsupported cross-repo、无法判定 closed state 等“系统不能安全前进”的情况；不会因为上游 `status:failed` 就向整条链路自动扩散。

## Migration Path

### v0.1

1. 增加 issue body YAML dependency syntax parser。
2. 增加 SQLite dependency tables 与 reconcile queue。
3. 在 poller 后接入 dependency resolver。
4. 增加 dependency-resolver agent，用 schedule-driven repair loop 消费 queue。
5. 在 state machine / router 前加 dependency hard gate。
6. 提供基础 CLI / HTTP graph 查询。

### v0.2

1. 支持跨仓库 dependency 读取。
2. 视需要补充 scheduler/config surface，使 dependency-resolver agent 不必依赖内嵌 maintenance loop。
3. 把 dependency resolve 做成 event-driven + poll-repair 双通道。
4. 视情况接 GitHub native relations 作为 declaration import source，但不替代 YAML canonical source。

### 对现有仓库的影响

这是纯 additive migration：

- 不写 dependency block 的 issue，行为不变。
- 已有 workflow / agent 配置不需要立即迁移。
- 老 issue 只要不声明 dependency，就不会触发 blocked 逻辑。
