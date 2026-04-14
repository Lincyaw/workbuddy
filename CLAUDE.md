# Workbuddy

GitHub Issue-driven agent orchestration platform. A self-hosted Go binary that polls GitHub Issues, matches them to agent definitions via labels, executes agents (locally or via GitHub Actions), and manages the full issue lifecycle through a label-based state machine.

## Architecture

```
GitHub Issues (Control Plane)
    │
    ├── workbuddy serve (dev env)  — human creates issues
    ├── workbuddy serve (dev agent env)  — picks up issues, implements, opens PRs
    ├── workbuddy serve (test env)  — runs tests, reports results
    └── workbuddy serve (prod env)  — handles deployment
```

Each environment runs its own `workbuddy serve` instance. The binary both polls for work and serves an HTTP audit UI for inspecting agent sessions.

## Tech Stack

- Language: Go
- CLI framework: cobra
- Config format: Markdown + YAML frontmatter (`.github/workbuddy/`)
- GitHub interaction: `gh` CLI
- No external database — local JSONL event logs

<!-- auto-harness:begin -->
## North-star targets

1. **Issue-to-PR closure rate** — % of agent-assigned issues that result in a merged PR (currently: unmeasured)
   Measure: `gh issue list --label status:done --json number | jq length` vs total assigned
   Mechanism: script

2. **Agent reliability** — % of agent runs that complete without error (currently: unmeasured)
   Measure: `grep -c '"status":"completed"' .workbuddy/logs/*.jsonl` vs total runs
   Mechanism: script

3. **State machine correctness** — 0 issues stuck in intermediate states > 1 hour (currently: unmeasured)
   Measure: `workbuddy status --stuck`
   Mechanism: script

Secondary: simpler code that maps clearly to requirements > clever abstractions.

## Dev-loop stages

| Stage | Command | Notes |
|-------|---------|-------|
| Build | `go build ./...` | Must compile cleanly |
| Test | `go test ./... -count=1` | Run after every change |
| Vet | `go vet ./...` | Run after every change |
| Lint | `staticcheck ./...` | If installed |
| Measure | `python ~/.autoharness/domains/softdev/scripts/validate_index.py project-index.yaml` | Compare to baseline |

## Iteration tracking

- Progress log: `progress.tsv` — dev-loop records keep/discard decisions and metric values
- Decision log: `decisions.md` — long-horizon logs autonomous decisions (L2+)

## Project conventions

- Package manager: Go modules
- Use `gh` CLI for all GitHub API interactions (not raw HTTP calls)
- Config files live in `.github/workbuddy/` — Markdown with YAML frontmatter
- Local state/logs live in `.workbuddy/` (gitignored)
- Errors: fail fast, wrap with `fmt.Errorf("context: %w", err)`
- No global state — pass dependencies explicitly
- Commit messages: imperative mood, reference issue number when applicable
- Branch strategy: `main` is protected, feature branches for all work

## Requirements index (MANDATORY)

This project uses `project-index.yaml` as the single source of truth for all requirements.
Every code change MUST keep the index synchronized:

1. **Before implementing**: find the matching requirement in `project-index.yaml`. If none exists, add one first.
2. **After implementing**: update the requirement's `code` paths and set `status: implemented`.
3. **After adding tests**: update the requirement's `tests` paths and set `status: tested`.
4. **After refactoring**: update any affected `code`/`tests` paths if files were moved or renamed.
5. **Never skip**: a code change without the corresponding index update is incomplete work.

Validate with: `python ~/.autoharness/domains/softdev/scripts/validate_index.py project-index.yaml`

## Active skills

- guide — project methodology briefing at session start
- dev-loop — complete dev cycle: implement, test, vibe-verify, AI-review, measure
- north-star — quantifiable optimization targets with observation mechanisms
- long-horizon — autonomous decision-making with escalation ladder (L1-L5)
- notify — push iteration reports via email/Feishu/Telegram
- new-project — spec-driven development for greenfield projects
<!-- auto-harness:end -->
