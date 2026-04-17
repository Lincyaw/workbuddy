---
name: workbuddy-guide
description: "Explain how to use workbuddy: single-process serve mode, distributed coordinator+worker mode, multi-repo setup, common operations, and troubleshooting. Use when the user says 'how to use workbuddy', 'workbuddy guide', 'how does workbuddy work', 'teach me workbuddy', 'workbuddy help', '怎么用workbuddy', '使用指南', or asks about deployment, running, or operating workbuddy."
user_invocable: true
---

# Workbuddy Guide

Interactive skill that explains how to operate workbuddy — from first run to
multi-repo distributed deployment. Adapt the depth and detail to what the user
is actually asking about; don't dump the entire guide when they ask a focused
question.

## Bundled references (read when relevant)

- `references/new-repo-onboarding.md` — Step-by-step checklist for configuring
  a brand new repository to work with workbuddy. Read this when the user asks
  how to add a new repo or when troubleshooting initial setup.
- `references/known-pitfalls.md` — Lessons from real-world testing: SSH vs HTTPS,
  gitignore, conventional commits, lease expiry, coverage mismatch, and more.
  Read this when something goes wrong or when advising on best practices.
- `references/writing-good-issues.md` — How to write issues that agents can
  process successfully: AC format, label conventions, common mistakes. Read
  this when the user asks how to create issues or when agents keep failing.

## Core concept (always explain first if the user is new)

Workbuddy is a GitHub Issue-driven agent orchestration platform:

1. **Human** creates a GitHub issue with `## Acceptance Criteria` and labels
   `workbuddy` + `status:developing`
2. **Coordinator** polls GitHub, detects the issue, dispatches `dev-agent`
3. **dev-agent** (codex or claude-code subprocess) reads the issue, writes code,
   creates a PR, changes label to `status:reviewing`
4. **Coordinator** detects label change, dispatches `review-agent`
5. **review-agent** evaluates each acceptance criterion:
   - All pass → `status:done` (issue auto-closed)
   - Any fail → `status:developing` (back to dev-agent with feedback, max 3 retries)

```
developing ⇄ reviewing → done
   ↕
 blocked  (missing Acceptance Criteria — human fixes, flips back)
```

Only two agents exist: `dev-agent` and `review-agent`. Runtime (`claude-code`
or `codex`) is a config field, not a separate agent.

## Deployment modes

### Mode 1: Single-process (`serve`) — start here

Everything runs in one process. Best for local development and testing.

```bash
# Build
go build -o workbuddy .

# Run (from the repo root that has .github/workbuddy/ config)
./workbuddy serve --port 8090 --poll-interval 15s
```

Key flags:
| Flag | Default | Purpose |
|------|---------|---------|
| `--port` | 8080 | HTTP server (health, metrics, dashboard) |
| `--poll-interval` | 30s | How often to poll GitHub for changes |
| `--max-parallel-tasks` | auto (min(CPU, 4)) | Concurrent agent executions |
| `--config-dir` | `.github/workbuddy` | Config directory path |
| `--db-path` | `.workbuddy/workbuddy.db` | SQLite database |
| `--coordinator-api` | false | Also expose task claim API for remote workers |

What it starts internally:
- Poller → polls `gh issue list` / `gh pr list` every interval
- StateMachine → reacts to label changes, dispatches agents
- Router → builds task context (issue body, comments, PRs)
- Embedded Worker pool → runs agent subprocesses (codex/claude)
- HTTP server → health, metrics, dashboard, audit API

### Mode 2: Distributed (`coordinator` + `worker`)

Coordinator and Worker(s) run as separate processes, communicating via HTTP
long-poll. Use this when workers run on different machines or you need to
scale horizontally.

#### Step 1: Start the Coordinator

```bash
export WORKBUDDY_AUTH_TOKEN="your-secret-token"

./workbuddy coordinator \
  --listen 0.0.0.0:8081 \
  --config-dir .github/workbuddy \
  --poll-interval 15s \
  --auth
```

Key flags:
| Flag | Default | Purpose |
|------|---------|---------|
| `--listen` | `127.0.0.1:8081` | Listen address. Use `0.0.0.0:8081` for remote workers |
| `--auth` | false | Require `WORKBUDDY_AUTH_TOKEN` for all API calls |
| `--config-dir` | (none) | Bootstrap with a local repo config on startup |
| `--loopback-only` | false | Dev mode: skip auth on loopback |

The Coordinator exposes these APIs:
- `POST /api/v1/repos/register` — register a repo's config
- `GET  /api/v1/repos` — list registered repos
- `DELETE /api/v1/repos/{owner/name}` — deregister a repo
- `POST /api/v1/workers/register` — register a worker
- `GET  /api/v1/tasks/poll?worker_id=X&timeout=30s` — long-poll for tasks
- `POST /api/v1/tasks/{id}/result` — submit task completion
- `POST /api/v1/tasks/{id}/heartbeat` — keep lease alive
- `POST /api/v1/tasks/{id}/release` — release task back to queue
- `GET  /health` — health check with registered repo count
- `GET  /metrics` — Prometheus metrics

#### Step 2: Start Worker(s)

```bash
./workbuddy worker \
  --coordinator http://coordinator-host:8081 \
  --token "your-secret-token" \
  --runtime codex \
  --repo Owner/RepoName
```

Key flags:
| Flag | Default | Purpose |
|------|---------|---------|
| `--coordinator` | (required) | Coordinator base URL |
| `--token` | (required) | Bearer token matching Coordinator's `WORKBUDDY_AUTH_TOKEN` |
| `--runtime` | `claude-code` | Agent runtime: `claude-code` or `codex` |
| `--repo` | from config.yaml | Which repo this worker serves |
| `--role` | from agent configs | Comma-separated roles (e.g., `dev,review`) |

**Important:** The Worker must be run from a directory that has
`.github/workbuddy/agents/*.md` locally. The Coordinator only sends
`{task_id, repo, issue_num, agent_name}` — the Worker loads the full
agent prompt template from its local config.

Worker lifecycle:
1. Registers with Coordinator (`POST /workers/register`)
2. Prunes orphaned worktrees from prior crashes
3. Long-polls for tasks (`GET /tasks/poll`, 30s timeout)
4. On task received: creates isolated git worktree at `.workbuddy/worktrees/issue-N/`
5. Launches agent subprocess in the worktree directory
6. Sends heartbeat every 15s to keep the lease
7. On completion: submits result, cleans up worktree
8. On `Ctrl+C`: releases task back to queue (`POST /tasks/{id}/release`)

**Worktree isolation**: Each task runs in its own git worktree, so multiple
workers can safely process different issues in the same repo checkout without
git conflicts. The main branch is never modified by agents.

**Scaling with multiple workers**: To increase parallelism, start multiple
worker processes. Each independently polls and claims tasks:
```bash
for i in 1 2 3; do
  nohup ./workbuddy worker --coordinator http://coord:8081 \
    --token $TOKEN --repo Owner/Repo \
    > /tmp/worker-$i.log 2>&1 &
done
```

## Multi-repo setup

A single Coordinator can manage multiple repositories. Each repo gets its own
independent Poller + StateMachine. Workers declare which repo(s) they serve.

### Registering repos

**Method A — Bootstrap at startup (single repo)**

Pass `--config-dir` when starting the Coordinator. It reads the local
`.github/workbuddy/config.yaml` and auto-registers that repo.

**Method B — Dynamic registration (multiple repos)**

From any repo that has `.github/workbuddy/` config:

```bash
cd /path/to/repo-B
export WORKBUDDY_AUTH_TOKEN="your-secret-token"
workbuddy repo register \
  --coordinator http://coordinator-host:8081 \
  --token "$WORKBUDDY_AUTH_TOKEN"
```

This serializes the local config (config.yaml + agents + workflows) and POSTs
it to the Coordinator. The Coordinator then starts a dedicated Poller for that
repo.

**Method C — Direct API call**

```bash
curl -X POST http://coordinator-host:8081/api/v1/repos/register \
  -H "Authorization: Bearer $WORKBUDDY_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "repo": "Owner/RepoName",
    "environment": "dev",
    "agents": [...],
    "workflows": [...]
  }'
```

### Listing registered repos

```bash
curl -H "Authorization: Bearer $TOKEN" http://coordinator-host:8081/api/v1/repos | jq
```

### Deregistering a repo

```bash
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://coordinator-host:8081/api/v1/repos/Owner/RepoName
```

### Multi-repo Worker configuration

Each Worker declares which repo(s) it serves. There are two ways to bind repos:

**Method A — At startup (static)**

```bash
# Single repo (backward-compatible)
./workbuddy worker --coordinator http://coord:8081 --token $TOKEN --repo Owner/repo-1

# Multiple repos with explicit local paths
./workbuddy worker --coordinator http://coord:8081 --token $TOKEN \
  --repos "Owner/repo-1=/path/to/repo-1,Owner/repo-2=/path/to/repo-2"
```

**Method B — Dynamic binding (no restart needed)**

Every worker starts a local management server on a random port. You can
add/remove repo bindings at runtime:

```bash
# Add a new repo binding to a running worker
# (run from the worker's control directory — where .workbuddy/ lives)
./workbuddy worker repos add Owner/new-repo=/path/to/new-repo

# List current bindings
./workbuddy worker repos list

# Remove a binding
./workbuddy worker repos remove Owner/old-repo
```

The worker automatically re-registers with the coordinator when bindings
change, so it immediately starts receiving tasks for newly added repos.

The management server address is written to `.workbuddy/worker.addr`.
You can also call it directly via curl:

```bash
MGMT_ADDR=$(cat .workbuddy/worker.addr)

# Add binding
curl -X POST $MGMT_ADDR/mgmt/repos \
  -d '{"repo":"Owner/new-repo","path":"/path/to/new-repo"}'

# List bindings
curl $MGMT_ADDR/mgmt/repos

# Remove binding
curl -X DELETE $MGMT_ADDR/mgmt/repos/Owner%2Fnew-repo
```

### Adding a new repo (full workflow, no restart)

To add a brand-new repo to an already-running workbuddy deployment:

```bash
# 1. Register the repo config with the coordinator
cd /path/to/new-repo
workbuddy repo register \
  --coordinator http://coordinator-host:8081 \
  --token "$WORKBUDDY_AUTH_TOKEN"

# 2. Bind the repo to a running worker
cd /path/to/worker-control-dir
./workbuddy worker repos add Owner/new-repo=/path/to/new-repo
```

Both coordinator and worker pick up the change immediately — no restart
required.

## Prerequisites for any repo

Before workbuddy can manage a repo, it needs:

1. **GitHub labels** — create them with `gh label create` or use the
   `/setup-repo` skill:
   - `workbuddy` (trigger label, #5319E7)
   - `status:developing` (#1D76DB)
   - `status:reviewing` (#D93F0B)
   - `status:done` (#0E8A16)
   - `status:blocked` (#BFD4F2)

2. **Config files** in `.github/workbuddy/`:
   - `config.yaml` — repo name, poll interval, port
   - `agents/dev-agent.md` — dev agent prompt + triggers
   - `agents/review-agent.md` — review agent prompt + triggers
   - `workflows/default.md` — state machine definition

3. **`gh` CLI** authenticated with write access to the repo

4. **Agent runtime** installed: `claude` CLI (for claude-code) or `codex` CLI
   (for codex runtime)

Use `/setup-repo` to create all of the above automatically.

## Common operations

### Creating a workbuddy issue

```bash
gh issue create -R Owner/Repo \
  --title "Your task title" \
  --body '## Description
What needs to be done.

## Acceptance Criteria
- [ ] Criterion 1 (must be individually verifiable)
- [ ] Criterion 2
- [ ] Tests exist for the above' \
  --label "workbuddy,status:developing"
```

The `workbuddy` label opts the issue into the state machine.
The `status:developing` label triggers the dev-agent.

### Checking pipeline status

```bash
# Task queue
./workbuddy status --tasks

# Recent events
./workbuddy status --events --since 10m

# Watch a specific issue until completion
./workbuddy status --watch --issue 42 --timeout 30m

# Find stuck issues
./workbuddy status --stuck

# Automated diagnosis
./workbuddy diagnose
./workbuddy diagnose --fix   # auto-apply safe fixes
```

### Viewing session logs

```bash
# View agent execution logs for an issue
./workbuddy logs <issue-number>
```

### Recovering from failures

```bash
# After unclean shutdown: kill zombies, reset DB, prune worktrees
./workbuddy recover
```

### Cache invalidation (force re-poll)

```bash
# Force the poller to re-process an issue on the next cycle
./workbuddy cache-invalidate --repo Owner/Repo --issue 42
```

### Manual label intervention

```bash
# Restart a stuck issue
gh issue edit 42 -R Owner/Repo \
  --remove-label 'status:blocked' \
  --add-label 'status:developing'

# Force completion
gh issue edit 42 -R Owner/Repo \
  --remove-label 'status:reviewing' \
  --add-label 'status:done'
```

### Token management (distributed mode)

```bash
# Create a worker token
./workbuddy coordinator token create \
  --worker-id worker-1 --repo Owner/Repo --roles dev,review

# List tokens
./workbuddy coordinator token list

# Revoke a token
./workbuddy coordinator token revoke --worker-id worker-1
```

### Config validation

```bash
./workbuddy validate
```

## Endpoints cheat sheet

### serve mode (default port 8080)

| Endpoint | Purpose |
|----------|---------|
| `GET /health` | Health check |
| `GET /metrics` | Prometheus metrics |
| `GET /events` | Audit event list |
| `GET /issues/{repo}/{num}/state` | Issue state |
| `GET /tasks` | Task list |
| `GET /tasks/watch` | SSE stream for task completion |

### coordinator mode (default port 8081)

| Endpoint | Purpose |
|----------|---------|
| `GET /health` | Health + registered repo count |
| `POST /api/v1/repos/register` | Register repo config |
| `GET /api/v1/repos` | List repos |
| `DELETE /api/v1/repos/{owner/name}` | Deregister repo |
| `POST /api/v1/workers/register` | Register worker |
| `GET /api/v1/tasks/poll` | Long-poll for tasks |
| `POST /api/v1/tasks/{id}/result` | Submit result |
| `POST /api/v1/tasks/{id}/heartbeat` | Heartbeat |
| `POST /api/v1/tasks/{id}/release` | Release task |

### worker management server (random port, loopback only)

| Endpoint | Purpose |
|----------|---------|
| `GET /mgmt/repos` | List repo bindings |
| `POST /mgmt/repos` | Add repo binding `{"repo":"O/R","path":"/..."}` |
| `DELETE /mgmt/repos/{owner%2Fname}` | Remove repo binding |

Address is in `.workbuddy/worker.addr`. CLI: `workbuddy worker repos {list,add,remove}`.

## Configuring a new repository

For a complete step-by-step guide, read `references/new-repo-onboarding.md`.
Here's the quick summary:

1. Create 5 GitHub labels: `workbuddy`, `status:developing`, `status:reviewing`,
   `status:done`, `status:blocked`
2. Create `.github/workbuddy/` with `config.yaml`, `agents/dev-agent.md`,
   `agents/review-agent.md`, `workflows/default.md`
3. **Add `.workbuddy/` to `.gitignore`** (easy to forget — causes session file leaks)
4. Commit + push the config
5. Validate: `workbuddy validate` (run from repo root)
6. (Distributed mode) Register: `workbuddy repo register --coordinator URL --token TOKEN`
7. (Distributed mode) Bind worker — either start a new one:
   `workbuddy worker --coordinator URL --token TOKEN --repo Owner/Repo`
   or add to a running worker: `workbuddy worker repos add Owner/Repo=/path/to/repo`

The agent configs (`dev-agent.md`, `review-agent.md`) and workflow (`default.md`)
are generic — copy them from any existing workbuddy-managed repo. The only
repo-specific file is `config.yaml` (set `repo:` and `port:`).

Important: the worker must be started from the target repo's root directory
because it loads agent prompt templates from the local `.github/workbuddy/agents/`.

For known issues and debugging tips, read `references/known-pitfalls.md`.

## Troubleshooting

### Issue created but nothing happens

1. Check labels: must have BOTH `workbuddy` AND `status:developing`
2. Check poller is running: look for `[poller]` in logs
3. Check config: `./workbuddy validate`
4. Force re-poll: `./workbuddy cache-invalidate --repo Owner/Repo --issue N`

### Agent runs but doesn't change labels

The agent subprocess (`codex`/`claude`) is responsible for calling
`gh issue edit` to change labels. If it doesn't, the state machine stalls.
Workbuddy auto-detects this and redispatches after the stuck timeout.

Manual fix: change labels yourself with `gh issue edit`.

### Worker can't connect to Coordinator

1. Check Coordinator is listening on the right address (`0.0.0.0` not `127.0.0.1`)
2. Check token matches `WORKBUDDY_AUTH_TOKEN`
3. Check firewall/network allows the port

### Worker says "agent not found in local config"

The Worker needs `.github/workbuddy/agents/dev-agent.md` (and review-agent.md)
in its working directory. The Coordinator only sends the agent name, not the
full config.

### Too many retries / cycle limit reached

The default workflow allows 3 retry cycles (developing ↔ reviewing). After
that, the state machine records a `cycle_limit_reached` event. The issue
stays in its current state for human intervention.

To reset: manually change labels and use `cache-invalidate`.

## Environment variables

| Variable | Required | Purpose |
|----------|----------|---------|
| `WORKBUDDY_AUTH_TOKEN` | For `--auth` mode | Bearer token for coordinator API |
| `WORKBUDDY_SLACK_WEBHOOK_URL` | Optional | Slack notifications |
| `WORKBUDDY_FEISHU_WEBHOOK_URL` | Optional | Feishu/Lark notifications |
| `WORKBUDDY_TELEGRAM_BOT_TOKEN` | Optional | Telegram notifications |
| `WORKBUDDY_TELEGRAM_CHAT_ID` | Optional | Telegram chat target |
| `WORKBUDDY_SMTP_HOST` | Optional | Email notification SMTP host |
| `WORKBUDDY_SMTP_PORT` | Optional | SMTP port |
| `WORKBUDDY_SMTP_USERNAME` | Optional | SMTP auth |
| `WORKBUDDY_SMTP_PASSWORD` | Optional | SMTP auth |
| `WORKBUDDY_SMTP_FROM` | Optional | Sender address |
| `WORKBUDDY_SMTP_TO` | Optional | Recipient address |

## Related skills

- `/setup-repo` — create all config files and labels for a new repo
- `/pipeline-monitor` — watch the pipeline, detect stuck issues, apply fixes
