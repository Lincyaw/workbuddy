# Mismatch Docs

这里记录"当前代码"和"现有需求表达"之间仍然不一致的部分。

## 差异清单

| 文档 | 差异主题 | 涉及代码 |
| --- | --- | --- |
| （当前无未解决的 mismatch） | — | — |

## 已归档

- `docs/mismatch/codex-runtime-drift.md`（issue #8 已补齐 Codex 结构化 runtime 接入）
- 原 "catalog 登记 N 个 agent / 代码存在 M 个" 的差异：随 2026-04-15 agent 角色合并决策
  一并解决，见 `docs/decisions/2026-04-15-agent-role-consolidation.md`。

## 使用规则

- 这类差异在收敛前，不能直接拿来指导代码修改。
- 先决定要"改代码补齐"，还是"收窄文档回到事实"。
- 收敛完成后，把文档迁回 `implemented` 或 `planned`。
