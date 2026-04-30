---
name: deploy
description: "Workbuddy deployment topology guide — explains the 3-unit bundle (supervisor + coordinator + worker), the rolling-restart property, and the cutover from the legacy single-process `serve` install. Use when the user says 'how should I deploy workbuddy', 'install workbuddy as a service', 'switch to bundle', 'rolling restart', '部署 workbuddy', '升级到 bundle'."
---

# Deploy

See `docs/upgrade-v0.4-to-v0.5.md` and `workbuddy deploy install --help`.

## Topology

Workbuddy supports two systemd layouts:

- **Bundle (default since v0.5)** — three user units. The supervisor
  (`Type=notify`, `KillMode=process`, `Restart=always`) owns the agent
  subprocesses behind a unix-socket IPC, so the worker can be SIGKILL'd /
  restarted without killing in-flight Claude or Codex runs. The worker
  re-attaches over the socket and continues `events-v1.jsonl` from the
  right offset.
  - `workbuddy-supervisor.service`
  - `workbuddy-coordinator.service` (`After=workbuddy-supervisor.service`)
  - `workbuddy-worker.service`     (`After=workbuddy-supervisor.service`)
- **Legacy single-process `serve`** — one `workbuddy.service`. Preserved for
  one migration window and for local-dev convenience only. Does **not**
  preserve in-flight agent runs across restart.

## Greenfield install (recommended)

```bash
workbuddy deploy install --scope user \
  --working-directory "$PWD" \
  --env-file /home/<you>/.config/workbuddy/worker.env \
  --coordinator-args=--listen=127.0.0.1:8081 --coordinator-args=--auth \
  --worker-args=--coordinator=http://127.0.0.1:8081 \
  --worker-args=--token-file=/home/<you>/.config/workbuddy/auth-token \
  --worker-args=--repos=OWNER/REPO=$PWD
```

The `--bundle` flag is no longer required — it is accepted as a no-op alias
for backwards compatibility with existing scripts.

## Cutover from `serve` to bundle

```bash
systemctl --user stop  workbuddy.service
systemctl --user disable workbuddy.service
workbuddy deploy delete --name workbuddy --scope user
workbuddy deploy install --scope user --working-directory "$PWD" \
  --env-file <env-file> --coordinator-args=... --worker-args=...
```

`workbuddy deploy upgrade` (no flags) refuses to silently upgrade a legacy
`serve` install — it errors with a migration hint. Pass `--legacy-serve` if
you genuinely want to upgrade the legacy unit in place.

## Rolling-restart upgrade

```bash
workbuddy deploy upgrade --name workbuddy-worker --version vX.Y.Z
```

The supervisor stays up across the worker restart, agents keep running, and
the new worker re-attaches via the supervisor socket. This is the
operational property issue #281 standardized as the default.

## Flag detail

This skill is a signpost. For full flag detail use the CLI:

```bash
workbuddy deploy install --help
workbuddy deploy upgrade --help
workbuddy deploy uninstall --help
```
