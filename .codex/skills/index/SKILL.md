---
name: index
description: "Audit repository documentation against the real codebase and reorganize docs into three buckets: implemented, planned, and mismatch. Use this whenever the user asks to review docs for correctness, compare docs with code or git diff, reconcile stale design docs, rewrite project documentation structure, or maintain `project-index.yaml` as the source of truth for doc-to-code mapping."
---

# Doc Code Consistency

## Overview

Use this skill to keep repository documentation aligned with the implementation.

Treat documentation maintenance as a code review task with three outputs:

1. `implemented`: documents that already match the current code.
2. `planned`: forward-looking design documents that intentionally describe a target state.
3. `mismatch`: places where docs and code disagree and must be reconciled before the docs can guide development.

Always ground decisions in the current repository state, not in memory.

## Core Rules

- Read the code before trusting the docs.
- Read `project-index.yaml` before reorganizing documentation.
- Prefer classifying uncertain topics as `mismatch` rather than overstating implementation status.
- Preserve user changes already in the worktree or index; never revert unrelated edits.
- If old docs are being replaced, create the new canonical docs first, then delete or retire the superseded files.
- Keep the three-bucket structure simple and explicit; avoid mixing current behavior and future design in the same file.

## Required Inputs

At minimum, inspect these locations if they exist:

- `docs/`
- `project-index.yaml`
- relevant code paths referenced by the docs
- `git diff` and `git diff --cached` when the user asks to include pending changes

If the repo uses a different documentation root or index file, adapt to that structure and record the choice in the rewritten docs.

## Workflow

### 1. Build the inventory

Do the following first:

- list the docs under `docs/`
- read `project-index.yaml`
- scan old and new docs for code references
- inspect the referenced code, not just filenames
- include staged and unstaged changes when the user asks for a full consistency pass

Use `scripts/doc_inventory.py` to generate a baseline inventory and stale-reference report, then confirm the findings by reading the actual files.

### 2. Classify each document or topic

For every meaningful topic, decide which bucket it belongs to:

- `implemented`: behavior is already visible in the code and safe to treat as current truth
- `planned`: intentionally future-state, not yet implemented, but still a valid target design
- `mismatch`: docs and code diverge, or the doc mixes current truth with speculative behavior

Use these heuristics:

- if the code path exists and behavior is observable today, prefer `implemented`
- if the doc explicitly defines a migration target or vNext shape, prefer `planned`
- if the doc over-claims capability, references removed structure, or mixes old and new worlds, prefer `mismatch`

### 3. Rewrite into canonical docs

Create or update:

- `docs/index.md`
- `docs/implemented/index.md`
- `docs/planned/index.md`
- `docs/mismatch/index.md`

Then add focused docs under each bucket.

Keep each file narrow and stable:

- implemented docs should describe current behavior only
- planned docs should describe target behavior only
- mismatch docs should explain the exact drift and what needs to be reconciled

### 4. Update the index source of truth

Update the `documentation:` section in `project-index.yaml` so it maps:

- bucket definitions
- document paths
- status
- related code
- notes

If the requirements section also points to docs, refresh those links too.

### 5. Remove or retire superseded docs

Only after the new canonical docs exist:

- delete obsolete top-level design docs if the user asked for a full migration
- remove stale references to deleted docs
- make sure the new bucket docs are the only authoritative doc set

### 6. Validate the result

Before finishing, run these checks:

- validate `project-index.yaml` parses cleanly
- search for references to deleted doc paths
- check that each bucket index only points to existing files
- verify the main implementation claims against the code one more time

## Output Expectations

When you finish the task, report:

- what moved into `implemented`
- what moved into `planned`
- what moved into `mismatch`
- which old docs were removed
- whether `project-index.yaml` was updated successfully
- any residual ambiguity that still needs a product or engineering decision

## Suggested Commands

Prefer fast local inspection commands such as:

```bash
rg --files docs
rg -n "documentation:|requirements:" project-index.yaml
rg -n "docs/|internal/|cmd/|\.github/workbuddy/" docs project-index.yaml
python .codex/skills/doc-code-consistency/scripts/doc_inventory.py --root .
```

## Reference Material

Read `references/doc-model.md` before a large rewrite to keep the bucket semantics and migration rules consistent.
