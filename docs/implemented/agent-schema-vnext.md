# Agent Schema vNext

状态：implemented

## 已落地字段

当前 agent schema 已支持以下字段：

- `runtime`
- `policy`
- `prompt`
- `output_contract`
- `command`

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
- 现有 `command` 配置继续可用
- runtime 优先读取 `prompt`，缺失时回退到 `command`

## policy 行为

`policy` 由 runtime 翻译为底层执行参数：

- Claude runtime:
  `danger-full-access` 会映射到 `claude --dangerously-skip-permissions`
- Codex exec runtime:
  `sandbox` / `approval` / `model` 会映射到 `codex exec` flag
- `policy.timeout` 会同步到 agent 的运行超时

不支持的 policy 组合会在加载阶段直接报错。

## output_contract 行为

`output_contract` 当前支持：

```yaml
output_contract:
  schema_file: schemas/result.json
```

实现约束：

- `schema_file` 相对路径按 agent 文件所在目录解析
- loader 会在启动时确认 schema 文件存在
- launcher 仅在 agent 成功结束时校验最终输出
- 校验目标优先使用 `LastMessage`，否则回退到 stdout（会去掉 `WORKBUDDY_META` 块）
- 最终输出必须是合法 JSON，并满足声明的 JSON Schema
- 校验失败会让本次 agent run 返回错误，不再静默放过无效结构化输出

## command 与 prompt 的职责

当前实现边界：

- Claude runtime: `prompt` 存在时直接走 stdin prompt 模式
- Codex runtime: `prompt` 存在时直接作为 `codex exec` 的输入 prompt
- `command` 仍保留，兼容旧 agent 和旧测试数据

仓库内置 agent 已迁移到 canonical schema：

- `.github/workbuddy/agents/dev-agent.md`
- `.github/workbuddy/agents/review-agent.md`
- `.github/workbuddy/agents/codex-dev-agent.md`
- `.github/workbuddy/agents/codex-review-agent.md`
- `.github/workbuddy/agents/triage-agent.md`
- `.github/workbuddy/agents/docs-agent.md`
- `.github/workbuddy/agents/security-audit-agent.md`
- `.github/workbuddy/agents/dependency-bump-agent.md`
- `.github/workbuddy/agents/release-agent.md`

## 尚未做的事

下面这些仍属于后续迁移议题，不在当前实现范围内：

- 对 `command` 输出 deprecation warning
- 移除 `command` 字段
- 落地 long-lived `codex-appserver` runtime
