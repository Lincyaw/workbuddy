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

If you want to install this plugin outside the repository, follow the
home-local convention from the `plugin-creator` skill:

```bash
curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install-codex-plugin.sh | bash
```

The installer downloads the repository archive, copies `plugins/workbuddy`
into `~/plugins/workbuddy`, and merges the `workbuddy` entry into
`~/.agents/plugins/marketplace.json` without replacing unrelated local plugin
entries.

If you are iterating locally from a clone instead, regenerate the bundle first:

```bash
python3 scripts/sync_codex_plugin.py
```

The home-local marketplace convention is:

- plugin path: `~/plugins/workbuddy`
- marketplace path: `~/.agents/plugins/marketplace.json`
- marketplace entry source path: `./plugins/workbuddy`

Example:

```bash
mkdir -p ~/.agents/plugins ~/plugins
cp -R plugins/workbuddy ~/plugins/workbuddy
```

When copying manually, merge the `workbuddy` entry into your existing
`~/.agents/plugins/marketplace.json` instead of overwriting the file.

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
