# Workbuddy

GitHub Issue-driven agent orchestration platform. Agents pick up Issues, do the work, and change labels to hand off to the next agent — fully automated.

<p align="center">
  <img src="docs/architecture.svg" alt="Workbuddy Architecture" width="800"/>
</p>

## Install

### Binary

```bash
curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install.sh | bash
```

Or build from source:

```bash
go build -o workbuddy .
```

### Deploy as a service

Install the current `workbuddy` binary into a managed location and optionally
write a systemd unit in one step:

```bash
workbuddy deploy install \
  --name workbuddy \
  --scope user \
  --systemd \
  --working-directory "$PWD"
```

That writes a deployment manifest under the selected scope, so you can later
redeploy the current binary or upgrade to the latest GitHub release without
retyping the service definition:

```bash
workbuddy deploy redeploy --name workbuddy --scope user
workbuddy deploy upgrade --name workbuddy --scope user --version latest
```

Managed deployments can also be paused, resumed, or removed in place:

```bash
workbuddy deploy stop --name workbuddy --scope user
workbuddy deploy start --name workbuddy --scope user
workbuddy deploy delete --name workbuddy --scope user
```

`deploy delete` removes the recorded manifest and systemd unit, but leaves the
installed binary in place.

`deploy install` defaults to `workbuddy serve`, but you can record dedicated
distributed roles by passing the runtime command after `--`.

Coordinator service example:

```bash
sudo workbuddy deploy install \
  --name workbuddy-coordinator \
  --scope system \
  --systemd \
  --working-directory /srv/workbuddy \
  -- coordinator --listen 0.0.0.0:8081 --db /srv/workbuddy/.workbuddy/workbuddy.db
```

Worker service example:

```bash
sudo workbuddy deploy install \
  --name workbuddy-worker-dev \
  --scope system \
  --systemd \
  --working-directory /srv/workbuddy-worker \
  -- worker \
     --coordinator http://127.0.0.1:8081 \
     --token <worker-token> \
     --role dev \
     --repo owner/repo
```

This makes the split explicit:

- `workbuddy deploy install ...` with no trailing command => installs `serve`
- `workbuddy deploy install ... -- coordinator ...` => installs a coordinator
- `workbuddy deploy install ... -- worker ...` => installs a worker

### Claude Code Plugin

```bash
claude plugin marketplace add https://github.com/Lincyaw/workbuddy
claude plugin install workbuddy
```

### Codex Plugin

```bash
curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install-codex-plugin.sh | bash
```

This syncs the repo-packaged workbuddy skills into:

- `~/.codex/skills/`
- `~/.codex/.workbuddy-installed-skills.json`

Re-running the installer is idempotent:

- existing workbuddy-managed skills are overwritten in place
- newly added upstream skills are installed automatically
- removed upstream workbuddy skills are pruned by default

## Skills

After installing the plugin, the following skills are available in Claude Code:

| Skill | Trigger | What it does |
|-------|---------|--------------|
| `/workbuddy-guide` | "how to use workbuddy", "使用指南" | Explains deployment modes, operations, and troubleshooting |
| `/setup-repo` | "configure repo", "配置仓库" | Onboards a new repo: creates labels, agent configs, and workflows |
| `/pipeline-monitor` | "monitor pipeline", "监工" | Watches agent execution, diagnoses stuck issues |
| `/merge-flow` | "merge approved PRs", "批量合并" | Merges a batch of workbuddy PRs with conflict resolution |

## License

Apache-2.0
