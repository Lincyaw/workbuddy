# Pull-based CICD: `deploy watch` + `workbuddy-updater.service`

This page covers the operator side of the Phase 2 CICD loop introduced in
issues #224 / #237 / #238: how a NAT-bound deployment pulls new releases
from GitHub on a schedule, how to pause it, and how to roll back.

The pipeline has three pieces:

1. `release.yml` — produces the `vX.Y.Z` tarballs + `checksums.txt` on
   GitHub Releases (Phase 2.1, REQ-093).
2. `workbuddy deploy watch` — long-running poller that downloads the
   newest release, verifies the SHA256 against the published
   checksums file, atomically swaps the binary in place, and restarts
   listed systemd units (Phase 2.2, REQ-095).
3. `workbuddy-updater.service` — the systemd user unit that wraps
   `deploy watch` so the loop survives reboots and crashes (this page,
   Phase 2.3, REQ-097).

## One-shot bootstrap

After a fresh box has the binary installed once (manually or via
`workbuddy deploy install`), you can wire up the updater with a single
command:

```
workbuddy deploy install \
  --enable-updater \
  --updater-repo Lincyaw/workbuddy \
  --env-file /home/ddq/.config/workbuddy/worker.env
```

That call does the usual install (binary + manifest, optionally a
systemd unit) and additionally writes
`~/.config/systemd/user/workbuddy-updater.service`, runs `systemctl
--user daemon-reload`, and `enable --now`s the updater.

`--enable-updater` is **off by default**. The first deploy never silently
enables the auto-update loop. You opt in explicitly per host.

The updater unit itself looks like:

```ini
[Unit]
Description=Workbuddy workbuddy-updater (deploy watch)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/home/ddq
ExecStart="/usr/local/bin/workbuddy" "deploy" "watch" "--repo" "Lincyaw/workbuddy" "--interval" "5m" "--systemctl-scope" "user" "--restart-units" "workbuddy-coordinator.service,workbuddy-worker.service"
Restart=always
RestartSec=30s
EnvironmentFile=/home/ddq/.config/workbuddy/worker.env

[Install]
WantedBy=default.target
```

Defaults (override with the matching install flag):

| Flag | Default |
|------|---------|
| `--updater-repo` | `Lincyaw/workbuddy` |
| `--updater-interval` | `5m` |
| `--updater-restart-units` | `workbuddy-coordinator.service,workbuddy-worker.service` |
| `--updater-name` | `workbuddy-updater` |

The updater inherits the install's `--scope` (so a `--scope user`
install writes the updater under `~/.config/systemd/user/`; a
`--scope system` install writes it under `/etc/systemd/system/`). The
updater reuses every `--env-file` you passed to install — that is how
you supply a GitHub PAT.

## GitHub token: scope, location, why

`deploy watch` reads the GitHub Releases API and downloads release
assets. For a **public** repo no token is needed. For a **private** repo,
or if you want to avoid the 60 req/h unauthenticated rate limit, set:

```
GH_TOKEN=ghp_xxx       # preferred
# or GITHUB_TOKEN, GITHUB_OAUTH — any one is picked up
```

Required scope: **`Releases: read`** (GitHub fine-grained PAT) or the
classic `repo` scope's read-only subset. Nothing else. The updater never
writes to GitHub.

Where to put it: the file you point `--env-file` at. Example:

```
# /home/ddq/.config/workbuddy/worker.env
GH_TOKEN=ghp_xxx
```

That file is also typically the home of the worker's runtime
credentials (e.g. `CRS_OAI_KEY` for codex), so reusing it keeps the
operator surface flat. `chmod 600`.

## Pause the updater (don't auto-upgrade right now)

```
systemctl --user stop workbuddy-updater
```

This stops the running watcher but leaves the unit enabled. The next
`daemon-reload` or boot brings it back. To keep it stopped across
reboots:

```
systemctl --user disable --now workbuddy-updater
```

## Manual upgrade (skip the watcher)

Two equivalent paths:

1. Use the existing one-shot upgrade command (resolves the latest
   release synchronously, downloads, replaces the binary, restarts the
   service that the install manifest knows about):

   ```
   workbuddy deploy upgrade --name workbuddy --scope user
   workbuddy deploy upgrade --name workbuddy-coordinator --scope system --version v0.5.1
   ```

2. Trigger one cycle of the watcher without the loop:

   ```
   workbuddy deploy watch --repo Lincyaw/workbuddy --once
   ```

   Add `--dry-run` to see what *would* happen without downloading or
   restarting anything.

## Rollback

`deploy watch` always backs the previous binary up to
`<state-dir>/previous-binary` (default
`~/.local/state/workbuddy/updater/previous-binary`) before swapping
the new one in. To revert:

```
systemctl --user stop workbuddy-updater          # don't fight the watcher
workbuddy deploy rollback \
  --restart-units workbuddy-coordinator.service \
  --restart-units workbuddy-worker.service
```

Rollback toggles between the two binaries: running it twice puts you
back on the new release. After a successful rollback you usually want
the updater to stay stopped until the upstream issue is fixed:

```
systemctl --user disable --now workbuddy-updater
```

When the fix lands, re-enable:

```
systemctl --user enable --now workbuddy-updater
```

## What the watcher does NOT do

- It does not restart itself. If a release breaks `deploy watch`'s own
  flags, you have to roll back manually.
- It does not page anyone. The unit relies on `Restart=always` to
  recover from transient failures (network blips, GitHub 5xx). Real
  alerting is out of scope for v0.5; see #224 for the v0.6 plan.
- It does not coordinate across hosts. Each box runs its own updater
  independently. A botched release upgrades every host on its next tick;
  stage rollouts by version-pinning `--updater-repo` to a fork or by
  toggling the unit on subsets of hosts.

## Files & paths summary

| Path | Owner | Purpose |
|------|-------|---------|
| `~/.config/systemd/user/workbuddy-updater.service` | this issue | unit that runs `deploy watch` |
| `~/.local/state/workbuddy/updater/current-version` | `deploy watch` | last successfully installed version (`vX.Y.Z`) |
| `~/.local/state/workbuddy/updater/previous-binary` | `deploy watch` | backup used by `deploy rollback` |
| `/usr/local/bin/workbuddy` (default) | `deploy watch`, `deploy install` | the binary itself |
| `~/.config/workbuddy/worker.env` (typical) | operator | `GH_TOKEN`, runtime creds |

For the watcher's own flags (`--repo`, `--interval`, `--restart-units`,
`--state-dir`, `--binary`, `--systemctl-scope`, `--dry-run`, `--once`)
see `workbuddy deploy watch --help`.
