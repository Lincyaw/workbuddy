# Issue Dependency Mechanism

状态：planned

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

结果是：

- 只要 issue 带上 `status:developing`，dev agent 就会被 dispatch。
- 系统不知道“这个 issue 虽然在 developing，但应该等另一个 issue 先完成”。
- 也没有统一的 blocked 可见性、force override、cycle detection、或依赖图查询接口。

## 目标状态

v0.1 目标是引入“同仓库 issue 依赖”最小闭环：

1. issue body 中声明依赖。
2. poller 每轮解析 dependency graph。
3. coordinator 在 state machine dispatch 之前做 gating。
4. 若依赖未满足，则 issue 进入 `status:blocked`，并写入一条带 marker 的 managed comment。
5. 若依赖满足，则 coordinator 自动把 issue 从 `status:blocked` 恢复到其待恢复状态，并继续原有 agent 流程。
6. 若出现 cycle、非法引用、或无法判定的 closed state，则打 `needs-human` 并停止自动推进。

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

- 只解析第一个同时满足 `yaml` fence 且根键包含 `workbuddy.depends_on` 的代码块。
- `depends_on` 必须是字符串数组。
- 同仓库简写 `#26` 会被规范化成 `Lincyaw/workbuddy#26`。
- 显式全名 `owner/repo#123` 在 v0.1 允许写入，但若不是当前 repo，则标记为“declared but unsupported in v0.1”。
- 大小写敏感：键名必须是 `workbuddy.depends_on`，避免自然语言误判。
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
- Visible blocked state：`status:blocked` label
- Human override：`override:force-unblock` label
- Managed explanation：issue comment，带固定 marker：

```text
<!-- workbuddy:dependency-status -->
```

同一个 issue 只维护一条 dependency-status comment。内容更新而不是重复发新评论。

### SQLite 侧

新增两类持久化数据：

1. `issue_dependencies`

- `repo`
- `issue_num`
- `depends_on_repo`
- `depends_on_issue_num`
- `source_hash` 或 `declared_at`
- `status` (`active`, `unsupported`, `invalid_cycle`, `removed`)

2. `issue_dependency_state`

- `repo`
- `issue_num`
- `resume_label`，例如 `status:developing`
- `blocked_reason_hash`
- `override_active`
- `graph_version`
- `last_comment_marker_id` 或 comment id
- `last_eval_at`

设计原则：

- issue body 是 declaration 真源。
- SQLite 是解析缓存、blocked 恢复位点、comment 幂等锚点、以及 observability 查询索引。

## Satisfaction Semantics

依赖 A 对 B 来说“满足”定义为以下任一条件：

1. A 仍是 open issue，且当前 labels 包含 `status:done`
2. A 已关闭，并且是“通过 linked PR 关闭”

明确不算满足的情况：

- A 只有 `status:reviewing`
- A 进入 `status:failed`
- A 被人工直接 close，但没有 linked PR 证据
- A 不存在 / 无权限读取 / 跨仓库且 v0.1 不支持

对 closed issue 的判断，不能仅凭“issue 不在 open list 中”。resolver 需要按需调用单 issue 读取接口，确认：

- issue 当前 state
- `closedByPullRequestsReferences` 或等价字段

如果 closed 但无法确认是否由 linked PR 关闭，默认不解锁，并打 `needs-human`。

## Poller Contract

### 运行位置

dependency resolution 放在每轮 poll 的 issue/PR diff 之后、state machine dispatch 之前。

顺序：

1. poller 拉取 issues / PRs，更新 cache。
2. dependency resolver 重新解析当前 open issues 的 dependency declarations。
3. resolver 构建图并计算 blocked / ready / invalid。
4. resolver 执行 GitHub side effects：
   - 加 / 去 `status:blocked`
   - 更新 managed comment
   - 必要时加 `needs-human`
5. 对“原本想进 `status:developing`，但被 block”的 issue，阻止 dispatch。
6. 对“本轮 newly unblocked 且待恢复状态可运行”的 issue，把 label 从 `status:blocked` 恢复成原 `resume_label`，让现有 state machine 继续工作。

### 为什么不把逻辑塞进现有 state machine

当前 `internal/statemachine/statemachine.go` 的输入是假设“labels 已经代表真实可执行状态”。dependency gating 更像是一个在 dispatch 前裁剪可执行集合的协调层：

- 需要读取整个 repo 的 dependency graph，而不只是单 issue 当前 event
- 需要写 comment / blocked label
- 需要处理 body edits、closed deps、跨 issue fan-out

因此更适合做成 poller 后、router 前的 resolver。

## Blocked-State UX

### Label 规则

blocked issue 使用：

- 保留 workflow trigger label，如 `type:feature`
- 去掉待执行状态 label，例如 `status:developing`
- 加上 `status:blocked`

同时在 SQLite 记录 `resume_label=status:developing`。

这样做的原因：

- `status:blocked` 在 GitHub UI 明确可见。
- 不会误触发 `status:developing` 对应 agent。
- unblock 时可以无损恢复到原目标状态。

### Comment 模板

managed comment 至少包含：

- 当前依赖列表
- 每个依赖的状态：`done`, `open`, `failed`, `closed-without-linked-pr`, `unsupported-cross-repo`
- 当前是否由 `override:force-unblock` 绕过
- cycle / needs-human 提示（若存在）

comment 必须带 marker，并按内容 hash 幂等更新，禁止每轮 poll 重复新增。

## Auto-Unblock Strategy

### v0.1 选择：polling-driven

v0.1 采用 polling-driven unblock，而不是 GitHub webhook / event-driven。

原因：

- 当前系统已经以 poller 为协调中枢，加入 dependency resolver 的最小改动最小。
- 所有 unblock 条件本来就依赖 open issues、closed issue 复查、PR 状态读取，轮询天然能收敛。
- 幂等更简单；重启 coordinator 后下一轮 poll 直接重算全图即可恢复。

代价：

- unblock 延迟最多一个 poll interval。
- 大仓库下每轮计算要注意缓存和增量读取。

v0.2.x 可以把“依赖 issue 进入 `status:done` / 被 linked PR 关闭”升级为事件触发，但 polling 仍保留为兜底修复环。

## Dispatch Gating

在 dispatch 前做一个硬门：

- 如果 issue 有未满足 dependency，`dispatchAgent(...)` 必须拒绝派发，并返回“blocked by dependency”。
- 如果 issue 正在运行 agent 时，它的某个 dependency 被重新打开或从 `status:done` 回退，当前运行任务不强制 kill；但 issue 完成后不得继续自动推进到下一步，必须重新进入 blocked 计算。

这个策略避免“依赖闪断”导致同一 issue 上下抖动和取消风暴。

## Manual Override

v0.1 的唯一 override 信号是保留 label：

- `override:force-unblock`

行为：

- 只对当前 issue 生效，不向下游传播。
- resolver 看到该 label 时，仍保留 dependency comment，但不施加 `status:blocked` gating。
- comment 明确记录“force-unblocked by human override”。
- 去掉该 label 后，下一轮 poll 恢复正常 dependency 计算。

选择 label 而不是 comment directive 的原因：

- label 更显眼，且现有系统已经是 label 驱动。
- label 的存在性天然幂等，不需要 comment 去重与解析优先级。

## Idempotency Model

必须同时保证以下操作可重复执行而无副作用：

- poller 重跑
- coordinator 重启
- issue body 被重复保存
- 依赖满足后多轮重复 unblock

具体规则：

1. declaration 解析结果规范化后写入 SQLite，只有 graph hash 变化才更新状态。
2. managed comment 通过 marker + comment id 定位，内容 hash 未变则不更新。
3. `status:blocked` / `override:force-unblock` 的 label reconcile 采用“目标态收敛”，不是“看见一次事件就 append 一次动作”。
4. unblock 恢复只在“当前有 `status:blocked` 且 `resume_label` 缺失于 labels”时执行一次。
5. router 在最终 dispatch 前再次读取 dependency snapshot，避免“同 tick 内刚解锁又被另一边重新 block”的竞态。

## Cycle Detection

### 算法

对当前 repo 的 open issues 构建有向图 `issue -> dependency`，每轮解析后执行 DFS / Tarjan 均可；v0.1 推荐 DFS with color marking，因为实现简单且足够。

输出：

- cycle 上的所有节点
- cycle path，例如 `#28 -> #26 -> #27 -> #28`

### 触发时机

- 理想情况：issue body 更新后的下一轮 poll 立即发现
- 最晚：在 issue 即将 dispatch 之前再次复核

### 发现后动作

- 给 cycle 涉及 issue 加 `needs-human`
- 保持或切换到 `status:blocked`
- 更新 dependency-status managed comment，写明 cycle path
- 不自动 force-break cycle

这样满足“最迟 dispatch 前必须发现”，并避免 silent deadlock。

## Race Conditions

### 两个 dependency 在同一 tick 同时满足

resolver 以“本轮全图快照”计算 ready 集合。只要所有 deps 在同一快照里都满足，就只做一次 unblock。

### dependency 在 agent 已运行时满足或失效

- 满足：不影响当前运行任务，最终由下一轮常规计算决定是否继续推进。
- 失效：不取消当前任务，但 issue 后续自动推进会被阻断，直到依赖再次满足。

### dep 被关闭后又重新打开

- reopened issue 在下轮 poll 中重新进入 open graph。
- 所有依赖它的 issue 重新计算并回到 `status:blocked`，除非有 `override:force-unblock`。

### coordinator 多实例 / 重启

SQLite 中的 dependency state + GitHub comment marker 让 reconcile 是幂等的；重复实例最多重复尝试同一目标态，不应产生多条 comment 或多次 dispatch。

## Failed / Retry Interaction

依赖 issue 进入 `status:failed` 时，默认视为“未满足且需要人工判断”，因此：

- dependent issue 继续 blocked
- dependency-status comment 明确标记 upstream failed
- dependent issue 不自动传播为 failed
- 如需继续推进，由人类在 upstream 修复后完成，或在 downstream 手工加 `override:force-unblock`

原因是：

- failed 只是“当前自动流程停止”，不是“依赖逻辑完成”
- 自动把失败向下游传播会制造过度放大和难以恢复的状态雪崩

## Cross-Repo Extension Path

v0.1：

- 语法允许 `owner/repo#123`
- resolver 识别并存储
- 但默认标记为 `unsupported-cross-repo`
- dependent issue 进入 blocked + comment，等待 v0.2 实现

v0.2.x 扩展点：

- GHReader 增加按 repo 批量查询 issue / PR 的能力
- store 主键统一使用 `(repo, issue_num)`
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
- blocked
- blocked_reasons
- resume_label
- override_active
- cycle
- last_evaluated_at

数据源优先来自 SQLite 的 dependency tables，而不是临时重新爬 GitHub。

## Concrete Code Touch Points

这部分只列将来实现会改哪里，不代表本 issue 要改代码。

### `internal/poller/`

- `internal/poller/poller.go`
  需要在 issue diff 后把 issue body 解析结果交给 dependency resolver，而不是只看 labels。
- `cmd/serve.go`
  需要把 dependency resolver 接到 poll loop 与 state machine 之间。

### `internal/store/`

- `internal/store/store.go`
  需要新增 dependency graph、dependency state、managed comment anchor 等表和 CRUD。
- `internal/store/types.go`
  需要新增 dependency edge / dependency status 的 typed models。

### `internal/statemachine/`

- `internal/statemachine/statemachine.go`
  需要在 dispatch 前接受“blocked verdict”，并在最终 dispatch 前做一次防竞态复核。
- 现有 `DispatchRequest` 可能需要携带 dependency snapshot version。

### `internal/router/`

- `internal/router/router.go`
  需要在真正写入 task queue / 下发 worker 前再次检查 blocked 状态，避免 stale ready verdict。

### GitHub read/write boundary

- `cmd/serve.go` 里的 `GHCLIReader`
  需要新增单 issue 读取 closed state / linked PR 信息的能力。
- 需要一个统一的 GitHub writer 来 reconcile:
  - `status:blocked`
  - `needs-human`
  - `override:force-unblock` 读取
  - dependency-status managed comment 的 create / update

### 可视化 / 调试

- `internal/webui/handler.go`
  后续可把 blocked graph 和 reason 暴露到 UI / API。

## Rejected Alternatives

### 方案 A：只靠 label 表达依赖

例如 `blocked-by:#26`、`blocked-by:#27`。

放弃原因：

- GitHub label 名过长且数量不稳定
- 多依赖、跨仓库和 cycle 定位都很难表达
- 解析和人类编辑都脆弱

### 方案 B：只靠 comment directive

例如人工评论 `/depends-on #26 #27`。

放弃原因：

- comment 流是 append-only，难定义“当前真值”
- 删除 / 编辑 / 多条指令冲突时语义复杂
- poller 当前也不以 comment 为主索引

### 方案 C：直接用 GitHub native sub-issues / Projects relation

放弃原因：

- 当前 workbuddy 主要依赖 repo 级 issue + `gh` CLI；引入 Projects 会扩大权限和运维面
- API / CLI 兼容性和脚本稳定性不如 issue body 中的显式 YAML
- 不利于离线审查和 code-review 中直接看到 dependency contract

## Distance From Current Code

这是一项中等偏大的协调层改造，不是“加一个 label 判断”。

距离主要在四处：

1. 现在系统按单 issue event 驱动，dependency 需要 repo 级 graph 视角。
2. 现在 Go 侧几乎不做 GitHub 写回；dependency 设计要求写 blocked label 和 managed comment。
3. 现在 store 没有 dependency 持久化模型。
4. 现在 dispatch 只受 workflow state 驱动，没有“前置条件未满足”这一层硬门。

但它仍然是增量式改造，因为：

- 无依赖 issue 完全走现有路径
- workflow schema 可以保持不变
- agent prompt / runtime / reporter 不需要先改

## Migration Path

### v0.1

1. 增加 issue body YAML dependency syntax。
2. 增加 SQLite dependency tables。
3. 在 poller 后接入 dependency resolver。
4. 支持同仓库 dependency gating、blocked label、managed comment、override label。
5. 提供基础 CLI / HTTP graph 查询。

### v0.2

1. 支持跨仓库 dependency 读取。
2. 把 dependency resolve 做成 event-driven + poll-repair 双通道。
3. 视情况接 GitHub native relations 作为 declaration import source，但不替代 YAML canonical source。

### 对现有仓库的影响

这是纯 additive migration：

- 不写 dependency block 的 issue，行为不变。
- 已有 workflow / agent 配置不需要立即迁移。
- 老 issue 只要不声明 dependency，就不会触发 blocked 逻辑。
