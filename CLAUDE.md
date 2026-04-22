# Workbuddy

GitHub Issue-driven agent orchestration platform. Hub-Spoke architecture: a Coordinator polls GitHub Issues and manages the label-based state machine; Workers execute agent instances (Claude Code, Codex, etc.). Agents follow the **Agent-as-Router** pattern (LangGraph-style) — each agent decides the next state by modifying issue labels via `gh issue edit`.

The catalog is deliberately minimal: only two roles exist, `role:dev` and `role:review`. No `test-agent`. Tests are covered by acceptance criteria in the issue; dev-agent produces tests as part of its artifact; review-agent verifies them. See `docs/decisions/2026-04-15-agent-role-consolidation.md`.

## Architecture

```
                    Coordinator (公网)
                    ├── GitHub Poller (gh CLI)
                    ├── State Machine (label-driven, cycle limits)
                    ├── Task Router (repo + role)
                    ├── Worker Registry
                    ├── SQLite persistence
                    └── HTTP API
                         │
                    ┌────┴────┐
                    │         │
                 Worker A  Worker B
                 repo:X    repo:Y
                 role:dev  role:dev
                 role:     role:
                 review    review
                 runtime:  runtime:
                 claude    codex
```

v0.1.0: `workbuddy serve` runs Coordinator + Worker in a single process (channel communication).
v0.2.0: `workbuddy coordinator` + `workbuddy worker` as separate processes (HTTP long-polling).

## Tech Stack

- Language: Go
- CLI framework: cobra
- Config format: Markdown + YAML frontmatter (`.github/workbuddy/`)
- GitHub interaction: `gh` CLI
- Database: SQLite (modernc.org/sqlite, pure Go, no CGO)
- Agent execution: Multi-runtime (Claude Code, Codex) instances launched as subprocesses

<!-- auto-harness:begin -->
## North-star targets

### Primary targets (development phase)

1. **Requirements coverage** — % of scoped requirements with status >= implemented
   - v0.1.0: 13/13 ✅ · v0.2.0: 13/13 ✅ · v0.3.0: 4/4 ✅
   - Measure: `grep -c 'status: implemented\|status: tested' project-index.yaml`
   - Mechanism: script (validate_index.py)

2. **Test coverage** — % of scoped requirements with status = tested
   - v0.1.0: 13/13 ✅ · v0.2.0: 13/13 ✅ · v0.3.0: 4/4 ✅
   - Measure: `grep -c 'status: tested' project-index.yaml`
   - Mechanism: script + `go test -cover ./...`

3. **Build health** — go build + go vet + go test all pass with 0 errors
   - Target: always green after every commit
   - Measure: `go build ./... && go vet ./... && go test ./... -count=1`
   - Mechanism: dev-loop gate

### Runtime targets (live deployment)

4. **End-to-end cycle success** — Agent-as-Router flow completes without manual intervention
   - v0.1.0 ✅ (first full cycle achieved)
   - v0.2.0 target: >= 50% of issues complete autonomously
   - Measure: issues with `status:done` vs total assigned

5. **Agent reliability** — % of agent runs that complete without error
   - v0.2.0 target: >= 80%
   - Measure: SQLite query on events table (completed vs dispatch counts)

6. **State machine correctness** — 0 issues stuck in intermediate states > 1 hour
   - v0.2.0 target: 0 stuck issues in 24h test run
   - Measure: `workbuddy status --stuck`

7. **Retry effectiveness** — % of retried issues that eventually reach done (not failed)
   - v0.2.0 target: >= 60%
   - Measure: SQLite query on transition_counts + issue final status

### Secondary principle

Simpler code that maps clearly to requirements > clever abstractions.

## Version milestones

### v0.1.0 — 单机合体 (MVP) ✅ shipped
**Status**: 13/13 requirements `tested` in `project-index.yaml`. `workbuddy serve` runs the full Issue → Agent → label → next Agent cycle end-to-end; SQLite tracks transitions, retries, events.
**Scope**: REQ-001~006, REQ-009, REQ-010, REQ-013, REQ-017, REQ-023, REQ-024, REQ-030

### v0.2.0 — 分布式 + 可观测性 ✅ shipped (2026-04-22)
**Status**: 13/13 requirements `tested`. `workbuddy coordinator` + `workbuddy worker` run as separate processes with HTTP long-polling and shared-token auth; multi-repo binding, full CLI surface (`init/status/run/validate/logs/coordinator/worker/recover/diagnose`) functional. Currently deployed on a single host via systemd user units; no code change is required to split across machines (swap `--coordinator http://…` + expose port).
**Scope**: REQ-007, REQ-008, REQ-011, REQ-018~022, REQ-025~029

### v0.3.0 — 高级编排 ✅ shipped
**Status**: 4/4 requirements `tested`: workflow engine, parallel dispatch, issue dependency graph, dashboard API.
**Scope**: REQ-012, REQ-014~016

### v0.4.0 — 扩展能力与运维加固 ✅ shipped (2026-04-22)
**Status**: REQ-031..060 全部 `tested`（26 条）。覆盖：issue dependency mechanism、recover/diagnose/cache-invalidate/metrics/admin restart-issue 等运维命令、scoped PAT permissions、stale-agent inference、dynamic worker repo binding、issue author allowlist、per-issue claim token fencing、coordinator claim recovery + stale-claim sweep、multi-repo dynamic registration、reporter comment overflow guard、dispatch cap on consecutive failures，以及 2026-04 的 codex app-server singleton multiplexing、worker SessionManager wiring（修死锁）、post-merge stale claim sweep 等修复。
**Scope**: REQ-031~060 (26 additional requirements)

## Dev-loop stages

| Stage | Command | Notes |
|-------|---------|-------|
| Build | `go build ./...` | Must compile cleanly |
| Test | `go test ./... -count=1` | Run after every change |
| Vet | `go vet ./...` | Run after every change |
| Lint | `golangci-lint run ./...` | Config: .golangci.yml |
| Measure | `python3 ~/.autoharness/scripts/validate_index.py project-index.yaml` | Compare to baseline |

## Iteration tracking

- Progress log: `progress.tsv` — dev-loop records keep/discard decisions and metric values
- Decision log: `decisions.md` — long-horizon logs autonomous decisions (L2+)

## Project conventions

- Package manager: Go modules
- Use `gh` CLI for all GitHub API interactions (not raw HTTP calls)
- Config files live in `.github/workbuddy/` — Markdown with YAML frontmatter
- Local state/logs live in `.workbuddy/` (gitignored)
- Database: `.workbuddy/workbuddy.db` (SQLite, WAL mode)
- Errors: fail fast, wrap with `fmt.Errorf("context: %w", err)`
- No global state — pass dependencies explicitly
- Commit messages: imperative mood, reference issue number when applicable
- Branch strategy: `main` is protected, feature branches for all work
- Merge strategy: **squash + rebase only** — keep `main` linear. No merge commits. Merge PRs with `gh pr merge --squash --rebase` (or GitHub UI "Squash and merge"). When updating a feature branch from `main`, use `git rebase main`, not `git merge`.
- Release tags: after merging user-facing changes to `main`, move the release tag (`git tag -f vX.Y.Z && git push --force origin vX.Y.Z` for the current patch, or cut a new `vX.Y.(Z+1)` tag) so `claude plugin update workbuddy` and `workbuddy deploy upgrade` pick the fix up. Skill/CLI/deploy/plugin edits all count as user-facing.
- Testing strategy: unit tests use mock/fake for `gh` CLI calls; integration tests use real `gh` against a test repo
- File naming: Go standard — lowercase, underscore separated, `_test.go` suffix for tests
- Agent config: `command` field MUST include routing instructions (gh issue edit for label changes)
- Agent config: `runtime` field selects execution backend (claude-code | codex), default claude-code
- GH call boundary: Go code only reads GitHub (Poller: gh issue/pr list) and writes comments (Reporter: gh issue comment). Label changes are done by agent subprocesses themselves via `gh issue edit`. Do not add GH write operations to Go code for label manipulation.
- Session audit: Agent execution produces session artifacts (conversation logs, tool call history) that must be captured and stored alongside stdout/stderr

## Requirements index (MANDATORY)

This project uses `project-index.yaml` as the single source of truth for all requirements.
Every code change MUST keep the index synchronized:

1. **Before implementing**: find the matching requirement in `project-index.yaml`. If none exists, add one first.
2. **After implementing**: update the requirement's `code` paths and set `status: implemented`.
3. **After adding tests**: update the requirement's `tests` paths and set `status: tested`.
4. **After refactoring**: update any affected `code`/`tests` paths if files were moved or renamed.
5. **Never skip**: a code change without the corresponding index update is incomplete work.

Validate with: `python3 ~/.autoharness/scripts/validate_index.py project-index.yaml`

## Active skills

- guide — project methodology briefing at session start
- dev-loop — complete dev cycle: implement, test, vibe-verify, AI-review, measure
- north-star — quantifiable optimization targets with observation mechanisms
- long-horizon — autonomous decision-making with escalation ladder (L1-L5)
- notify — push iteration reports via email/Feishu/Telegram
- new-project — spec-driven development for greenfield projects
<!-- auto-harness:end -->
