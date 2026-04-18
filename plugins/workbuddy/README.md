# Workbuddy Plugin for Codex

Codex plugin for operating [workbuddy](https://github.com/Lincyaw/workbuddy) — a GitHub Issue-driven agent orchestration platform.

## Included skills

| Skill | Purpose |
| --- | --- |
| `workbuddy-guide` | Explain deployment modes, operating model, and troubleshooting flow |
| `setup-repo` | Configure a repository for workbuddy orchestration |
| `pipeline-monitor` | Inspect stuck or unhealthy agent execution pipelines |
| `merge-flow` | Merge a batch of approved workbuddy PRs with design-intent checks |

## Source of truth

This Codex plugin is generated from the Claude plugin content in `.claude/plugins/workbuddy/`.
Run `python3 scripts/sync_codex_plugin.py` after updating the Claude plugin files.
