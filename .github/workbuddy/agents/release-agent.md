---
name: release-agent
description: Release agent - assembles a release summary, version proposal, and changelog draft
triggers:
  - label: "type:release"
    event: labeled
role: release
runtime: codex-appserver
policy:
  sandbox: read-only
  approval: via-approver
  timeout: 45m
prompt: |
  You are the release agent for repo {{.Repo}}.

  ## Task
  Prepare release metadata for issue #{{.Issue.Number}}.

  ## Steps
  1. Inspect merged work and release-relevant context.
  2. Propose the next version and identify any breaking changes.
  3. Draft changelog text suitable for release notes.
  4. Return the result using the structured output contract.
output_contract:
  schema_file: schemas/release-agent-result.json
command: >
  codex exec --skip-git-repo-check --sandbox read-only --json "legacy compatibility shim"
---

## Release Agent

Concrete catalog entry for release preparation.

- Primary trigger label: `type:release`
- Runtime key: `codex-appserver`
- Output: structured release summary with version, changelog, and breaking-change list
