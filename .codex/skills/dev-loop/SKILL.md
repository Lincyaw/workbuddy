---
name: dev-loop
description: Use when implementing a feature, fix, refactor, or content change that should go through a complete delivery loop: implement, test, vibe-check, AI review, and measurement before keeping or discarding the change. Trigger when the user asks to build something, fix a bug, write or revise content, improve quality, or when you need to decide whether a change is actually ready to ship. 中文触发：开发、实现、修复、写代码、改一下、做这个功能、加个功能、修个 bug、帮我写、帮我改、重构。
---

# Dev Loop

Run a complete development loop instead of stopping at "tests pass". The loop is:

`implement -> test -> vibe-check -> AI review -> measure -> keep or discard`

Use it for code, docs, papers, content, and other artifacts where correctness alone is not enough. The goal is to catch regressions early, verify real-world quality, and make keep/discard decisions using explicit observations rather than gut feel alone.

## Modes

Choose the mode that matches the task:

- **Metric-driven mode**: use when the project already has measurable targets or observable indicators. The agent can iterate semi-autonomously and keep/discard changes based on measurements.
- **Human-in-the-loop mode**: use when subjective quality is important, such as UX, writing, or API ergonomics. The user vibe-checks each meaningful iteration before you proceed.

Both modes use the same stages. The difference is who gates progression.

## The stages

### 1. Implement

- Make the change following local conventions.
- Before making a risky change, create a clean rollback point with git.
- Keep the project's goals or north-star targets in mind while designing the change.

### 2. Test

- Run the relevant automated checks.
- If the change is not covered, add or update tests.
- Do not move on with broken tests.

Tests answer: "Does it do what I intended?"

### 3. Vibe-check

Experience the change like a user or consumer would. This catches unspecified but obvious problems.

Examples:

- Frontend: open the flow, click through, resize, try keyboard navigation.
- Backend/API: hit real endpoints, inspect response shapes, try malformed input.
- CLI: run real commands with both valid and invalid inputs.
- Library: write a small consumer example and judge ergonomics.
- Docs/paper: read it cold and check flow and clarity.
- Data pipeline: run sample data and inspect outputs.

Guidance:

- In metric-driven mode, self-verify where possible and ask for human spot checks periodically.
- In human-in-the-loop mode, explicitly tell the user what to check once tests pass.

### 4. AI review

Do an objective alignment pass, not just a style review.

Check:

- **Consistency**: naming, patterns, structure, style
- **Scope**: minimal change, no speculative abstractions
- **Trade-offs**: does the change improve or preserve the project's key goals?
- **Simplicity**: all else equal, prefer the simpler implementation

### 5. Measure

Run the project's observations and compare against a baseline or the previous successful iteration.

Typical observation types:

- **Script observations**: run every iteration when cheap and deterministic
- **Agent observations**: run periodically for qualitative aspects
- **Human observations**: use at milestones or when uncertainty remains

Possible outcomes:

- **Keep**: indicators improved or held steady and no stage found blocking issues
- **Discard**: indicators regressed without a stronger compensating gain
- **Investigate**: mixed results or the signal is too weak to call

If you discard, revert to the pre-change snapshot and try a different approach instead of patching blindly.

## Intensity guide

Adjust how heavy the loop is based on the change:

- **Trivial change**: test and measure only
- **Bug fix**: full loop
- **New feature**: full loop, usually human-in-the-loop
- **Refactor**: test, AI review, and measure; lighter vibe-check unless UX changes
- **Performance work**: full loop plus benchmark-style measurement
- **Docs/paper section**: vibe-check, AI review, and whatever quality measurement exists

## Loop-back rules

- Test failure -> go back to testing after the fix
- Cosmetic vibe issue -> fix and re-check vibe
- Structural issue -> return to implementation and then re-test
- AI review concern -> fix and re-review
- Metric regression -> usually discard and rethink the approach
- Too many iterations on the same problem -> step back and reconsider the strategy

## Logging and tracking

When the project uses iteration tracking, keep a simple progress log such as `progress.tsv` with:

- date
- commit or snapshot id
- status (`baseline`, `keep`, `discard`, `investigate`)
- key indicators
- short description

Use a human-readable location in the repo root unless the project specifies another path.

If your environment includes telemetry tooling such as skill-evolve, log the keep/discard result as a best-effort step after measurement. Do not let telemetry block the actual work.

## How this skill connects to others

- `north-star`: defines the goals or indicators that measurement should check
- `long-horizon`: governs how to make autonomous decisions during extended loops
- `notify`: can report flagged decisions or iteration outcomes without blocking the work

## Migration notes

This Codex version keeps the procedural guidance from the Claude skill but removes Claude-specific frontmatter fields such as `category`. Any project-specific commands should live in the repo's own instructions or adjacent references, not hard-coded into this skill.
