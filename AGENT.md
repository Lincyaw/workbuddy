# Workbuddy Agent Guide

This repository is a Go development repo for `workbuddy`, an issue-driven agent orchestration platform. Use this file as the lightweight development-facing agent contract. The broader project narrative and architecture context remain in `CLAUDE.md`.

## Core Principles

Three axioms govern all work. Fall back to these when task-specific instructions do not cover a situation.

### 1. Quality over quantity

A small number of things done well beats a large number done poorly.

- Tests: each test must justify its existence by verifying a distinct, user-visible behavior. Trivial, duplicated, parser-only, and wiring-only tests are not allowed unless they are the only realistic way to guard a production failure.
- Observations: prefer a few meaningful metrics over vanity dashboards.
- Code: lines of code are cost, not value; the best code is code you do not write.
- Documents: short docs that get read beat comprehensive docs that do not.
- Experiments and ideas: one validated, well-designed loop beats many vague attempts.

The standard for "enough" is simple: if you cannot explain why an item exists and what would be lost without it, there are probably too many items. For tests in particular, the default bar is stricter: keep only high-signal end-to-end or behavior-level tests where a failure means the product is actually broken.

### 2. Surface problems early

Expose errors at the earliest possible moment.

- Fail fast instead of hiding errors behind silent fallbacks.
- Validate assumptions before expensive work.
- Outline before drafting; check structure before polishing.
- Prefer checks that run continuously over checks that run only after a large batch of work.
- Do not hide real complexity just to make something look simpler.

Visible complexity is manageable. Hidden complexity becomes operational debt.

### 3. Deliberate execution

Every decision should be traceable to a reason.

- Understand before acting.
- Validate manually before automating.
- Measure before optimizing.
- Consider removing before adding.
- When simplifying, state where the complexity moved and why that tradeoff is acceptable.

## Development Defaults

When making changes in this repo, optimize for shipping reliable Go changes with clear requirement traceability.

- Language and tooling: Go modules, Cobra CLI, SQLite via `modernc.org/sqlite`, GitHub access through `gh` CLI.
- Primary quality gate: `go build ./... && go test ./... -count=1 && go vet ./...`
- Lint gate: `golangci-lint run ./...`
- Requirement sync gate: `python3 ~/.autoharness/scripts/validate_index.py project-index.yaml`
- Config source of truth: `.github/workbuddy/`
- Local runtime state: `.workbuddy/`
- Errors: wrap with context via `fmt.Errorf("context: %w", err)`
- Dependency style: avoid global state; pass dependencies explicitly.

## Development Conventions

- Use `gh` for GitHub operations instead of hand-rolled API calls.
- Keep `project-index.yaml` synchronized with implementation and tests for every requirement-related change.
- Preserve the agent routing model: agents drive label transitions themselves; Go code should not take over that responsibility unless the design changes explicitly.
- Prefer small, reviewable changes that map cleanly back to requirements and tests.
- Testing policy is strict: every retained or added test must cover necessary end-to-end behavior, and a red result must indicate a meaningful product regression.
- Do not add or keep tests that only restate flag parsing, string formatting, helper passthroughs, JSON field plumbing, or other trivial implementation details unless that path is itself the shipped contract.
- If several tests exercise the same behavior, collapse them into the smallest suite that still protects the real failure mode.
- Keep `main` linear; rebase feature branches instead of merging `main` into them.
- Treat session artifacts, logs, and audit outputs as first-class debugging evidence.

## Before Calling Work Done

Run the relevant parts of this loop:

1. Build and test the touched area.
2. Run `go vet` and lint when the change is non-trivial.
3. Remove or consolidate low-signal tests introduced by the change so the suite stays behavior-focused.
4. Check whether docs in `README.md`, `docs/`, or `CLAUDE.md` need to move with the code.
5. Summarize the behavioral change, the verification performed, and any remaining risk.
