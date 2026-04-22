# Worker Agent Resolution Is Per Bound Repo

- Date: 2026-04-22
- Status: accepted

## Context

`workbuddy worker` already supports multiple `--repos owner/name=/path`
bindings and can add/remove bindings at runtime via the loopback
management API.

Before issue #164, the worker loaded exactly one config directory during
startup and then resolved every claimed task against that single
`cfg.Agents` map. In a multi-repo worker this meant:

- repo A could silently run repo B's command for the same agent name
- repo-specific agents could be reported as missing even though they
  existed in that repo's own `.github/workbuddy/`
- runtime behavior depended on the worker's startup directory instead of
  the task's bound repo path

That violates REQ-049's repo-specific path routing guarantee and makes
multi-repo workers unsafe.

## Decision

Keep multi-repo workers supported, and resolve configuration per bound
repo.

Concretely:

- each bound repo loads its own config from `<binding-path>/.github/workbuddy/`
  when `configDir` is repo-relative
- task execution resolves `task.AgentName` against the config stored for
  `task.Repo`, not a shared global agent catalog
- when bindings change through the worker management API, the worker
  reloads the bound repo configs before re-registering with the
  coordinator
- a missing agent error must name the repo so operators can fix the
  correct checkout

## Rationale

This keeps the existing multi-repo feature usable without introducing a
second worker mode or an operator surprise. It also matches the mental
model already implied by `--repos`: the binding path is the source of
truth for that repo's worktree and its workbuddy config.

Rejecting multi-repo startup would be simpler in the short term, but it
would effectively roll back REQ-049's shipped binding model and break the
current management API story.

## Consequences

- Multi-repo workers remain supported.
- Repo-relative `configDir` values are interpreted relative to each bound
  repo path, not the worker control directory.
- Tests must cover two bound repos defining different commands for the
  same agent name and verify repo-specific resolution.
- Worker registration derives implicit roles from the union of all bound
  repo configs when `--role` is not provided.
