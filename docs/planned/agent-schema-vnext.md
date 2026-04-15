# Agent Schema vNext

状态：planned

## 设计目标

新的 agent schema 不再只把 frontmatter 当成“命令拼装配置”，而是升级为“声明式执行合同”。

目标字段分层：

- `runtime`: 选择运行时后端
- `policy`: 统一描述 sandbox、approval、model、timeout
- `prompt`: 为长驻或结构化 runtime 预留模板入口
- `output_contract`: 约束结构化结果
- `command`: 在过渡期继续作为 one-shot runtime 的 canonical 入口

## vNext 示例

```markdown
---
name: codex-dev-agent
description: Development agent for feature implementation
role: dev
runtime: codex-appserver
triggers:
  - label: "status:developing"
    event: labeled

policy:
  sandbox: danger-full-access
  approval: never
  model: gpt-5.4
  timeout: 30m

prompt: |
  You are a development agent for repo {{.Repo}}.
  Issue #{{.Issue.Number}}: {{.Issue.Title}}

output_contract:
  schema_file: schemas/dev-result.json
---
```

## 兼容原则

v0.1.x 的兼容边界必须写死：

- 现有 `command` 配置继续可用
- `runtime` 为空仍默认 `claude-code`
- 对外公开 runtime key 继续保留 `claude-code`、`codex`
- `claude-oneshot`、`codex-exec` 更适合作为内部实现名，而不是立即暴露给全部已有配置

推荐映射：

| Agent runtime | 内部实现 |
| --- | --- |
| `claude-code` | `claude-oneshot` |
| `codex` | `codex-exec` |
| `codex-appserver` | `codex-appserver` |

## 为什么不引入 routing 字段

这个设计延续当前 workbuddy 的核心前提：

- agent 自己决定下一条边
- label 仍由 agent 自己通过 `gh issue edit` 修改
- Go 侧只做校验、审计、记录

因此 schema 不引入单独的 `routing` 声明块来接管状态机语义。

## command 与 prompt 的职责边界

过渡期建议明确：

- one-shot runtime：`command` 是真源
- long-lived runtime：可直接消费 `prompt`
- `policy` 由 runtime 解释成底层参数

如果未来要把 `prompt` 升级成 canonical 字段，需要单独补迁移文档，不能在当前实现里隐式切换。

`command` 的 deprecation 节奏建议：

- v0.1.x：`command` 与 `prompt` + `policy` 并存，runtime 优先读 `prompt`，回退 `command`
- v0.2.0：`command` 触发 deprecation 警告，文档迁移完成
- v0.3.0：移除 `command` 字段

## policy 字段枚举

`policy` 字段是 runtime 无关的统一抽象。runtime 实现负责把这些值翻译成自己的 flag / RPC 字段。

### `policy.sandbox`

| 值 | 语义 |
| --- | --- |
| `read-only` | 只读，不允许写文件、不允许网络 |
| `workspace-write` | 仅当前 workspace 可写，网络受限 |
| `danger-full-access` | 全权限，等同主机用户 |

### `policy.approval`

| 值 | 语义 |
| --- | --- |
| `never` | 不询问，全部按 sandbox 静态规则放行/拒绝 |
| `on-failure` | 沙箱阻挡时才询问 |
| `on-request` | 模型主动请求时才询问 |
| `via-approver` | 走 `Approver` 接口（仅 long-lived runtime 支持） |

### `policy.model`

字符串，runtime 自己解释。空值表示用 runtime 默认。

### `policy.timeout`

Go duration 字符串（如 `30m`）。

### Runtime 支持矩阵

| policy | claude-oneshot | codex-exec | codex-appserver |
| --- | --- | --- | --- |
| `sandbox: read-only` | ✅ 默认行为 | ✅ `--sandbox read-only` | ✅ `sandbox:{type:"readOnly"}` |
| `sandbox: workspace-write` | ⚠️ Claude 无对应概念，降级为 `read-only` 并在 audit 警告 | ✅ `--sandbox workspace-write` | ✅ `sandbox:{type:"workspaceWrite"}` |
| `sandbox: danger-full-access` | ✅ 默认行为 | ✅ `--sandbox danger-full-access` | ✅ `sandbox:{type:"dangerFullAccess"}` |
| `approval: never` | ✅ | ✅ exec 模式默认 | ✅ `approvalPolicy: "never"` |
| `approval: on-failure` | ❌ 不支持，启动时报错 | ✅ `--ask-for-approval on-failure` | ✅ |
| `approval: on-request` | ❌ 不支持，启动时报错 | ✅ | ✅ |
| `approval: via-approver` | ❌ 不支持，启动时报错 | ❌ 不支持 | ✅（Approver 接口） |

不支持的 policy 值在 agent 加载阶段就报错，不会延迟到运行时。
