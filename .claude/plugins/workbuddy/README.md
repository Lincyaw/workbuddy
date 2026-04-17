# Workbuddy Plugin

Claude Code plugin for operating [workbuddy](https://github.com/Lincyaw/workbuddy) — a GitHub Issue-driven agent orchestration platform.

## Skills

| Skill | Trigger | Purpose |
|-------|---------|---------|
| `/workbuddy-guide` | "how to use workbuddy", "使用指南" | Concepts, deployment modes, operations, troubleshooting |
| `/setup-repo` | "configure repo", "配置仓库" | Onboard a new repo: labels, config, agents, workflow |
| `/pipeline-monitor` | "monitor pipeline", "监工" | Watch agent execution, diagnose stuck issues |

## Quick start

1. Set up a repo: `/setup-repo Owner/Repo`
2. Learn the basics: `/workbuddy-guide`
3. Monitor execution: `/pipeline-monitor Owner/Repo 37`

## Prerequisites

- `gh` CLI authenticated with repo access
- `codex` or `claude` CLI installed
- workbuddy binary built (`go build -o workbuddy .`)
