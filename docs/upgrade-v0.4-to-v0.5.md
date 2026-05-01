# Upgrading workbuddy from v0.4.x to v0.5.0

v0.5 splits the worker into two processes:

- **worker** (stateless) — long-polls the coordinator and forwards events. Can be SIGKILL'd at any time without losing in-flight agent runs.
- **agent supervisor** (long-lived) — owns claude-code / codex subprocesses behind a unix-socket IPC API. Survives worker restarts.

This means a v0.4 host that ran two systemd user units (`workbuddy-coordinator.service` + `workbuddy-worker.service`) needs a third one (`workbuddy-supervisor.service`) before the new worker binary can dispatch agents.

## Pre-flight

1. Drain in-flight tasks if you can — `workbuddy status --stuck` should be empty before you start. v0.4 workers tear down their agent subprocess on SIGTERM, so anything mid-flight at upgrade time is wasted budget.
2. Take a snapshot of `~/.config/workbuddy/deployments/` and `.workbuddy/workbuddy.db` (the WAL-mode SQLite). The migration adds rows; it does not migrate schema destructively, but a snapshot makes rollback to v0.4 trivial.
3. Confirm the env file you used for the v0.4 worker is reachable. The supervisor unit reuses it (it does not need its own secrets, but inheriting `XDG_RUNTIME_DIR` etc. via `EnvironmentFile=` is convenient).

## Upgrade steps (recommended path)

```bash
# 0) stop the v0.4 worker so it doesn't pick up new tasks during the swap
systemctl --user stop workbuddy-worker.service

# 1) install the v0.5 binary in place
workbuddy deploy upgrade --name workbuddy-worker --version v0.5.0
workbuddy deploy upgrade --name workbuddy-coordinator --version v0.5.0

# 2) add the supervisor unit (idempotent — picks up the env-file from the
#    coordinator/worker manifests, writes Type=notify, KillMode=process,
#    Restart=always, ExecStart=workbuddy supervisor). Since v0.5.x this
#    happens by default for any `deploy upgrade`; the --bundle alias is
#    accepted for compatibility with existing automation.
workbuddy deploy upgrade --name workbuddy-worker --version v0.5.0
# (or, equivalently, run `deploy install` with --bundle-skip-coordinator
#  and --bundle-skip-worker; both paths converge on the same supervisor unit.)

# 3) start the supervisor first, then the worker
systemctl --user daemon-reload
systemctl --user enable --now workbuddy-supervisor.service
systemctl --user start workbuddy-worker.service

# 4) verify
workbuddy supervisor --help          # binary exposes the new subcommand
systemctl --user status workbuddy-supervisor.service workbuddy-worker.service
ls "$XDG_RUNTIME_DIR/workbuddy-supervisor.sock"  # unix socket created by supervisor
```

The coordinator can be left running through the upgrade — it has no stateful link to the supervisor, only to the worker over HTTP.

## Greenfield install (v0.5)

```bash
workbuddy deploy install --scope user \
  --env-file /etc/workbuddy/bundle.env \
  --coordinator-args=--listen=127.0.0.1:8081 --coordinator-args=--auth \
  --worker-args=--coordinator=http://127.0.0.1:8081 --worker-args=--token=$WORKBUDDY_TOKEN \
  --worker-args=--repos=owner/repo=/srv/workbuddy
```

`deploy install` defaults to the bundle layout: `workbuddy-supervisor.service`
(`Type=notify`, `KillMode=process`, `Restart=always`), then
`workbuddy-coordinator.service` and `workbuddy-worker.service` with
`After=workbuddy-supervisor.service` ordering. Trailing `-- args` are not
allowed in bundle mode; use the per-unit
`--{supervisor,coordinator,worker}-args` flags instead (each repeatable).
The `--bundle` flag is accepted as a no-op alias for backwards compatibility.

To remove a bundled install:

```bash
workbuddy deploy uninstall --scope user --force
```

This stops/disables/deletes all three units, removes their manifests, and (unless `--keep-state` is set) wipes `$XDG_STATE_HOME/workbuddy` and the supervisor unix socket. The on-disk `workbuddy` binary is left in place.

## Rollback to v0.4

If the new worker misbehaves, you can roll back without touching the coordinator:

```bash
# 1) stop the v0.5 worker + supervisor
systemctl --user stop workbuddy-worker.service workbuddy-supervisor.service

# 2) reinstall the v0.4 binary on top of the worker manifest
workbuddy deploy upgrade --name workbuddy-worker --version v0.4.x

# 3) start the v0.4 worker
systemctl --user start workbuddy-worker.service

# 4) (optional) leave the supervisor unit in place — disabled, it is harmless.
#     Or remove it explicitly:
systemctl --user disable --now workbuddy-supervisor.service
rm -f ~/.config/systemd/user/workbuddy-supervisor.service
rm -f ~/.config/workbuddy/deployments/workbuddy-supervisor.json
systemctl --user daemon-reload
```

The v0.4 worker speaks the original "spawn agent in-process" path; it does not look at the supervisor socket. Coordinator schemas in v0.5 only add columns (e.g. `tasks.supervisor_agent_id`), so a downgraded v0.4 worker reads the legacy columns and ignores the new ones.

## What `--bundle` does *not* change

- The on-disk binary path (`/usr/local/bin/workbuddy` or `~/.local/bin/workbuddy`) is unchanged.
- `deploy upgrade` for a single named unit (`--name workbuddy-coordinator`) still runs the binary swap + restart on that one unit. `--bundle` is purely additive: it backfills the supervisor unit if a v0.4 install is detected.
- The supervisor unit deliberately uses `KillMode=process`. Restarting it (e.g. for a binary swap) leaves the agent subprocesses running, reparented to systemd, so the next supervisor invocation reattaches via the on-disk events log. A "drain" mode that waits for in-flight agents to finish before swapping the supervisor binary is intentionally deferred to v0.5.x.

## Upgrading to session-data-ownership refactor (v0.5.x → v0.6 candidate)

**Architecture change.** The `sessions` and `agent_sessions` tables now live on the **worker**, not the coordinator. The coordinator opens its DB via `NewCoordinatorStore`, which idempotently drops both tables on startup. Session reads on the coordinator (`/api/v1/sessions`, `/api/v1/sessions/{id}[/events|/stream]`, and the issue-detail session list) are served by `internal/sessionproxy` — a fan-out / reverse-proxy that resolves `session_id → worker_id → workers.audit_url` and forwards the request to the worker's audit HTTP server (started via `--audit-listen`, default `127.0.0.1:0`). See `internal/sessionproxy/resolver.go` for the routing rules.

**Deploy order: workers first, then coordinator.** The new coordinator's `dropLegacySessionTables` removes the rows it used to read from. If you upgrade the coordinator first while workers are still on the old binary:

- Old workers don't advertise `audit_url` → the coordinator records empty strings.
- The single-session resolver treats empty `audit_url` as "fall back to the coordinator-local handler" (the same code path used by `workbuddy serve`'s in-process worker). The fan-out listing path also includes the coordinator-local store as a single dedupe-friendly source when any worker has no `audit_url`.
- Result: sessions are still served via the coordinator-local fallback for `serve` installs and gracefully degraded for old workers — but live sessions written by the still-old workers go to the coordinator DB, are dropped at next coordinator restart, and appear as `worker offline` until you upgrade the worker. Upgrade workers first to skip the gap entirely.

`workbuddy serve` is unaffected. The in-process worker shares the coordinator's DB (one SQLite file, `NewStore` not `NewCoordinatorStore`), so the legacy session tables stay in place and the local fan-out fallback covers the empty-`audit_url` path.

**Rollback caveat.** Once `dropLegacySessionTables` has run on a coordinator DB, the rows are gone. Rolling the coordinator binary back to a pre-Phase-3 build does **not** require manual SQL: `createTables` uses `CREATE TABLE IF NOT EXISTS`, so the old binary recreates empty `sessions` and `agent_sessions` on next startup and resumes writing into them as before. The dropped rows from before the rollback are unrecoverable, but the DB is functional. If you want to preserve the historical rows across a Phase-3 rollout, snapshot `.workbuddy/workbuddy.db` (or the coordinator-side DB path) before the upgrade.

**`audit_url` scheme validation.** Worker registration now rejects `audit_url` values whose scheme is anything other than `http` or `https`, or whose host portion is empty. Empty `audit_url` continues to be accepted (it means "no audit listener configured" — sessionproxy falls back to the coordinator-local handler). Operators with custom registration tooling that previously sent placeholders like `none://disabled` must switch to either an empty string or a real `http://...` URL.
