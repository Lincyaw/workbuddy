# Workbuddy Plugin

Claude Code plugin for operating [workbuddy](https://github.com/Lincyaw/workbuddy) — a GitHub Issue-driven agent orchestration platform.

## Codex plugin

This Claude plugin is the source of truth for the generated Codex plugin bundle.

- generated Codex plugin: `plugins/workbuddy`
- generated Codex marketplace entry: `.agents/plugins/marketplace.json`
- Codex packaging and home-local install notes: `plugins/workbuddy/README.md`

After changing files in `.claude/plugins/workbuddy/`, regenerate the Codex
bundle with:

```bash
python3 scripts/sync_codex_plugin.py
```

## Skills

| Skill | Trigger | Purpose |
|-------|---------|---------|
| `/workbuddy-guide` | "how to use workbuddy", "使用指南" | Concepts, deployment modes, operations, troubleshooting |
| `/setup-repo` | "configure repo", "配置仓库" | Onboard a new repo: labels, config, agents, workflow |
| `/pipeline-monitor` | "monitor pipeline", "监工" | Watch agent execution, diagnose stuck issues |
| `/merge-flow` | "merge approved PRs", "批量合并" | Merge a batch of workbuddy PRs with conflict resolution |
| `/deploy` | "deploy workbuddy", "安装部署" | Bundle vs serve topology, systemd install, rolling restart |
| `/web-debug` | "verify frontend", "验下 webui" | End-to-end SPA validation with headless browser |

## Commands

| Command | Purpose |
|---------|---------|
| `handle-incident` | Diagnose, fix or escalate a single workbuddy alert (invoked by `operator-watch`) |

## Quick start

1. Set up a repo: `/setup-repo Owner/Repo`
2. Learn the basics: `/workbuddy-guide`
3. Monitor execution: `/pipeline-monitor Owner/Repo 37`

## Prerequisites

- `gh` CLI authenticated with repo access
- `codex` or `claude` CLI installed
- workbuddy binary built (`go build -o workbuddy .`)
