# Workbuddy Plugin for Codex

Codex plugin for operating [workbuddy](https://github.com/Lincyaw/workbuddy) — a GitHub Issue-driven agent orchestration platform.

## Layout

This repository keeps the generated Codex plugin in the repo-local layout
defined by the `plugin-creator` skill:

- plugin bundle: `plugins/workbuddy`
- manifest: `plugins/workbuddy/.codex-plugin/plugin.json`
- repo-local marketplace entry: `.agents/plugins/marketplace.json`

The marketplace entry points to `./plugins/workbuddy`, which is the expected
repo-relative shape for a local Codex plugin catalog.

## Regenerate

The Codex plugin is generated from the Claude plugin source tree.

```bash
python3 scripts/sync_codex_plugin.py
```

This updates:

- `plugins/workbuddy/`
- `.agents/plugins/marketplace.json`

CI enforces that these generated files stay in sync with
`.claude/plugins/workbuddy/`.

## Home-local install

If you want to use these skills with a local Codex CLI session, install them
directly into `~/.codex/skills`:

```bash
curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install-codex-plugin.sh | bash
```

The installer downloads the repository archive, copies each skill from
`plugins/workbuddy/skills/` into `~/.codex/skills/`, and records the managed
skill set in `~/.codex/.workbuddy-installed-skills.json`.

Upgrade behavior is intentionally idempotent:

- re-running the installer overwrites the same workbuddy skill directories
- newly added upstream workbuddy skills are installed automatically
- upstream workbuddy skills that were removed are pruned by default
- set `WORKBUDDY_KEEP_REMOVED=1` if you want to keep retired workbuddy skills locally

If you are iterating locally from a clone instead, regenerate the bundle first:

```bash
python3 scripts/sync_codex_plugin.py
```

Example:

```bash
mkdir -p ~/.codex/skills
cp -R plugins/workbuddy/skills/* ~/.codex/skills/
```

If you copy manually, keep in mind that future installer runs treat the
installer-managed set as authoritative and may prune workbuddy skills that no
longer exist upstream.

If your local Codex runtime needs additional skill registration beyond the
packaged plugin layout, treat that as runtime-specific setup rather than part
of the plugin bundle format.

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
