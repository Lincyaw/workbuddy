---
name: setup-repo
description: Configure a GitHub repository for workbuddy orchestration. Use when the user wants to set up repo automation, initialize workbuddy, create workbuddy labels/agents/workflows, onboard a new repository, or says phrases like "set up repo", "configure repo for workbuddy", "initialize workbuddy", "配置仓库", or "初始化workbuddy".
---

# Setup Repo

Bootstrap a repository so workbuddy can drive issue-based orchestration with GitHub labels, agent definitions, workflow files, and local runtime config.

This is a workflow skill. Use it when Codex should inspect a target repo, create or update `.github/workbuddy/` files, add `.workbuddy/` to `.gitignore`, and optionally create missing GitHub labels via `gh`.

## Quick workflow

1. Resolve the target repository.
   - Prefer an explicit `owner/repo` argument from the user.
   - Otherwise detect it with `gh repo view --json nameWithOwner -q .nameWithOwner`.
2. Inspect the current repo state.
   - Detect the stack from files such as `go.mod`, `package.json`, `pyproject.toml`, or `Cargo.toml`.
   - Check whether `.github/workbuddy/` already exists.
   - Read `.gitignore` before editing it.
   - Query existing labels with `gh label list --repo <repo> --json name -q '.[].name'`.
3. Create or update the local workbuddy files.
   - Add agent definitions under `.github/workbuddy/agents/`.
   - Add workflow definitions under `.github/workbuddy/workflows/`.
   - Add `.github/workbuddy/config.yaml`.
   - Add `.workbuddy/` to `.gitignore` if it is missing.
4. Create missing labels in GitHub only when `gh` is authenticated and the user wants the remote repo configured now.
5. Report exactly what changed locally and remotely, plus any skipped items.

## Decision rules

- Prefer updating existing workbuddy files over overwriting them blindly.
- Keep stack-specific guidance minimal in `SKILL.md`; read `references/templates.md` for the exact label list and starter templates.
- When the repo already contains agent definitions, preserve project-specific commands and only adjust the parts needed for workbuddy compatibility.
- If `gh` auth or repo access is missing, still prepare the local files and clearly note the remote label step was not completed.
- If the user asks about migrating Claude settings, explain that Codex skills cover reusable instructions, but Claude-style `hooks` from `.claude/settings*.json` do not have a direct per-skill equivalent in the Codex skill format used here.

## Claude-to-Codex migration notes

Migrate the Claude skill itself into this Codex skill format:
- keep only `name` and `description` in frontmatter
- move the procedural guidance into the Markdown body
- move detailed templates into `references/`
- add `agents/openai.yaml` for Codex UI metadata

Do not try to migrate `.claude/settings.json` or `.claude/settings.local.json` verbatim:
- the permission allowlists are Claude-specific runtime configuration, not skill content
- the `PostToolUse` hook for `Skill` is not part of the Codex `SKILL.md` contract
- if that hook behavior still matters, re-express it as explicit instructions, a helper script, or a plugin outside the skill itself

## Reference map

Read only what you need:
- `references/templates.md` -> required labels, agent templates, workflow templates, and config skeleton

## Output checklist

Before finishing, make sure the response includes:
- target repo used
- detected stack
- files created or updated
- labels created or skipped
- any manual follow-up needed, especially for remote GitHub steps
