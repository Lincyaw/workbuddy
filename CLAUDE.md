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

1. **Requirements coverage** — % of v0.1.0 requirements with status >= implemented
   - Baseline: 0/13 (0%)
   - v0.1.0 target: 13/13 (100%)
   - Measure: `grep -c 'status: implemented\|status: tested' project-index.yaml` for v0.1.0 scope
   - Mechanism: script (validate_index.py)

2. **Test coverage** — % of v0.1.0 requirements with status = tested
   - Baseline: 0/13 (0%)
   - v0.1.0 target: 13/13 (100%)
   - Measure: `grep -c 'status: tested' project-index.yaml` for v0.1.0 scope
   - Mechanism: script + `go test -cover ./...`

3. **Build health** — go build + go vet + go test all pass with 0 errors
   - Baseline: N/A (no code yet)
   - Target: always green after every commit
   - Measure: `go build ./... && go vet ./... && go test ./... -count=1`
   - Mechanism: dev-loop gate

### Runtime targets (measurable after v0.1.0 deploys)

4. **End-to-end cycle success** — Agent-as-Router flow completes without manual intervention
   - v0.1.0 target: >= 1 successful full cycle (Issue → Agent → label change → next Agent → done)
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

### v0.1.0 — 单机合体 (MVP)
**Goal**: 单机跑通 Issue → Agent 执行 → Agent 改 label → 下一个 Agent → 结果回写的完整闭环，含环重试。
**Scope**: REQ-001~006, REQ-009, REQ-010, REQ-013, REQ-017, REQ-023, REQ-024, REQ-030 (13 requirements)
**Exit criteria**:
- All 13 requirements status = tested
- `workbuddy serve` → create Issue + label → Agent executes → Agent changes label → next Agent triggered → cycle completes or hits retry limit
- `go build && go test && go vet` all pass
- SQLite correctly tracks transitions, retries, events

### v0.2.0 — 分布式 + 可观测性
**Goal**: Coordinator/Worker 分离，长轮询通信，多仓库支持，完整 CLI 工具集。
**Scope**: REQ-007, REQ-008, REQ-011, REQ-018~022, REQ-025~029 (13 requirements)
**Exit criteria**:
- `workbuddy coordinator` and `workbuddy worker` run on different machines
- Worker long-polls and executes tasks
- Multi-repo registration and isolation
- All CLI commands functional

### v0.3.0 — 高级编排
**Goal**: 多步骤 workflow、并行 agent、Issue 依赖管理。
**Scope**: REQ-012, REQ-014~016 (4 requirements)

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
