# Agent Schema vNext

状态：implemented

## 已落地字段

当前 agent schema 已支持以下字段：

- `runtime`
- `policy`
- `prompt`
- `output_contract`（历史字段，见下文）
- `command`（legacy shim，见下文）

对应代码：

- `internal/config/types.go`
- `internal/config/loader.go`
- `internal/launcher/process.go`
- `internal/launcher/codex.go`
- `internal/launcher/output_contract.go`

## 当前兼容边界

v0.1.x 当前实现是：

- `runtime` 为空时默认仍为 `claude-code`
- 对外 runtime key 继续接受 `claude-code`、`codex`、`codex-appserver`
- `codex` 在 loader 中规范化为内部实现名 `codex-exec`
- 现有 `command` 配置继续可用（仅为兼容历史数据）
- runtime 优先读取 `prompt`，缺失时回退到 `command`

## policy 行为

`policy` 由 runtime 翻译为底层执行参数：

- Claude runtime:
  `danger-full-access` 会映射到 `claude --dangerously-skip-permissions`
- Codex exec runtime:
  `sandbox` / `approval` / `model` 会映射到 `codex exec` flag
- `policy.timeout` 会同步到 agent 的运行超时

不支持的 policy 组合会在加载阶段直接报错。

注意：sandbox / approval 这类策略未来将迁移到 Coordinator 侧的动态分派，
根据 issue label 在调度时决定，而不再写死在 agent 定义里。详见
`docs/decisions/2026-04-15-agent-role-consolidation.md`。

## output_contract 行为（已在 2-agent catalog 中弃用）

`output_contract` 历史上用于声明 agent 最终输出必须满足的 JSON Schema：

```yaml
output_contract:
  schema_file: schemas/result.json
```

实现约束（仍保留在 loader / launcher 代码中以兼容历史数据）：

- `schema_file` 相对路径按 agent 文件所在目录解析
- loader 会在启动时确认 schema 文件存在
- launcher 仅在 agent 成功结束时校验最终输出
- 校验目标优先使用 `LastMessage`，否则回退到 stdout（会去掉 `WORKBUDDY_META` 块）
- 最终输出必须是合法 JSON，并满足声明的 JSON Schema
- 校验失败会让本次 agent run 返回错误，不再静默放过无效结构化输出

**为什么新的 2-agent catalog 不再使用 `output_contract`：**
dev-agent 的工件形态自由（代码、文档、依赖升级、报告、release notes 都算工件），
无法也不应预先约束成单一 JSON schema；review-agent 的输出是自然语言评审评论，
会通过 `gh issue comment` 直接落回 issue，不是结构化结果。因此新的内置 agent
不再声明 `output_contract`。字段本身保留是为了不破坏历史数据加载路径。

## command 与 prompt 的职责

当前实现边界：

- Claude runtime: `prompt` 存在时直接走 stdin prompt 模式
- Codex runtime: `prompt` 存在时直接作为 `codex exec` 的输入 prompt
- `command` 仍保留，兼容旧 agent 和旧测试数据

**`command` 是 legacy shim，已弃用：** 早期版本用 `command` 字段直接拼 shell 命令，
现在 agent 的执行入口统一是 `prompt`。保留 `command` 仅为让旧的测试 fixtures 和
已写入历史仓库配置的 agent 继续可加载，不鼓励在新 agent 配置中使用。后续会补
deprecation warning 并最终移除。

仓库内置 agent 已迁移到 canonical schema（只保留 2 个）：

- `.github/workbuddy/agents/dev-agent.md`
- `.github/workbuddy/agents/review-agent.md`

## 尚未做的事

下面这些仍属于后续迁移议题，不在当前实现范围内：

- 对 `command` 输出 deprecation warning
- 移除 `command` 字段
- 移除 `output_contract` 字段（等确认没有历史数据依赖后）
- 落地 long-lived `codex-appserver` runtime
- Coordinator 侧基于 issue label 的 sandbox / approval 动态分派
