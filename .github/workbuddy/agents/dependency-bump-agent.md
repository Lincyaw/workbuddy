---
name: dependency-bump-agent
description: Dependency bump agent - updates dependencies, validates the repo, and prepares a PR summary
triggers:
  - label: "type:deps"
    event: labeled
role: deps
runtime: codex
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 30m
prompt: |
  You are the dependency bump agent for repo {{.Repo}}.

  ## Task
  Update dependencies requested by issue #{{.Issue.Number}}, run the relevant validation, and prepare a structured change summary.

  ## Steps
  1. Inspect dependency manifests and determine the minimal safe bump set.
  2. Apply the updates and adjust lockfiles if needed.
  3. Run the repository validation required by the changed stack.
  4. Return a structured summary of updated modules, validation status, and PR readiness.
output_contract:
  schema_file: schemas/dependency-bump-agent-result.json
command: >
  codex exec --skip-git-repo-check --sandbox danger-full-access --json "legacy compatibility shim"
---

## Dependency Bump Agent

Concrete catalog entry for dependency maintenance work.

- Primary trigger label: `type:deps`
- Runtime: `codex` (`codex-exec` after loader normalization)
- Intended future extension: scheduler-based invocation in addition to label-triggered routing
