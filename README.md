# Workbuddy

GitHub Issue-driven agent orchestration platform.

Workbuddy uses a Hub-Spoke architecture: a **Coordinator** polls GitHub Issues and manages a label-based state machine; **Workers** execute agent instances (Claude Code, Codex, etc.). Agents follow the **Agent-as-Router** pattern — each agent decides the next state by modifying issue labels via `gh issue edit`.

```
                Coordinator
                ├── GitHub Poller
                ├─��� State Machine (label-driven)
                ├── Task Router
                ├── Worker Registry
                ├── SQLite persistence
                └── HTTP API
                     │
                ┌────┴───���┐
             Worker A   Worker B
             repo:X     repo:Y
```

## Install

### Binary

Download the pre-built binary for your platform:

```bash
curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install.sh | bash
```

Options via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `WORKBUDDY_VERSION` | `latest` | Pin a specific version (e.g. `0.1.0`) |
| `INSTALL_DIR` | `~/.local/bin` | Where to put the binary |
| `GITHUB_TOKEN` | _(auto-detect from `gh`)_ | GitHub token for API calls |

Or build from source:

```bash
git clone https://github.com/Lincyaw/workbuddy.git
cd workbuddy
go build -o workbuddy .
```

### Claude Code Plugin

Install the plugin directly from Claude Code:

```bash
claude plugin marketplace add https://github.com/Lincyaw/workbuddy
claude plugin install workbuddy
```

This gives you the following skills inside Claude Code:

| Skill | Trigger | Purpose |
|-------|---------|---------|
| `/workbuddy-guide` | "how to use workbuddy" | Deployment modes, operations, troubleshooting |
| `/setup-repo` | "configure repo" | Onboard a new repo with labels, config, agents |
| `/pipeline-monitor` | "monitor pipeline" | Watch agent execution, diagnose stuck issues |
| `/merge-flow` | "merge approved PRs" | Batch-merge workbuddy PRs with conflict resolution |

## Quick Start

1. **Initialize a repo** for workbuddy orchestration:

   ```bash
   workbuddy init --repo Owner/Repo
   ```

2. **Run in single-process mode** (Coordinator + Worker together):

   ```bash
   workbuddy serve --config .github/workbuddy/config.yaml
   ```

3. **Create an issue** with the appropriate labels, and workbuddy takes over:
   - `role:dev` — assigns a dev agent to implement the issue
   - `role:review` — assigns a review agent to check the PR
   - Agents change labels themselves to drive the state machine forward

## Deployment Modes

### Single-process (v0.1.0)

```bash
workbuddy serve --config .github/workbuddy/config.yaml
```

Coordinator and Worker run in the same process, communicating via channels. Good for getting started and small-scale use.

### Distributed (v0.2.0)

```bash
# On the coordinator machine (public-facing)
workbuddy coordinator --config .github/workbuddy/config.yaml --addr :8080

# On each worker machine
workbuddy worker --coordinator http://coordinator:8080 --repos Owner/Repo
```

Workers long-poll the Coordinator for tasks. Supports multiple repos and multiple workers.

## CLI Commands

| Command | Description |
|---------|-------------|
| `serve` | Run Coordinator + Worker in a single process |
| `coordinator` | Run the remote Coordinator HTTP API |
| `worker` | Run a standalone Worker (long-polls Coordinator) |
| `init` | Scaffold workbuddy config for a repository |
| `status` | Summarize issue status from the audit server |
| `diagnose` | Scan SQLite store for common pipeline failures |
| `logs` | Print session logs for an issue attempt |
| `recover` | Recover runtime state after unclean shutdown |
| `validate` | Validate config files and workflow references |
| `version` | Print version information |

## How It Works

1. **Poller** watches GitHub Issues for label changes
2. **State Machine** evaluates transition rules based on current labels
3. **Router** assigns the issue to a matching Worker based on repo and role
4. **Launcher** starts an agent subprocess (Claude Code or Codex)
5. **Agent** works on the issue, then changes labels via `gh issue edit` to signal completion
6. **State Machine** picks up the label change and triggers the next agent (or marks done)

Only two agent roles exist: `dev` and `review`. Runtime (Claude Code or Codex) is a config field, not a separate agent.

## Configuration

Config files live in `.github/workbuddy/` as Markdown with YAML frontmatter. See `workbuddy init` to generate the default config, or use the `/setup-repo` skill in Claude Code.

## Prerequisites

- Go 1.26+ (for building from source)
- `gh` CLI authenticated with repo access
- `claude` or `codex` CLI installed on worker machines

## License

MIT
