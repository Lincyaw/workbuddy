---
name: merge-flow
description: "Merge a batch of approved workbuddy PRs into main: order them, resolve conflicts against design intent, validate post-merge health, and report. Use when the user asks to 'merge the batch', 'merge approved PRs', 'integrate finished issues', '批量合并', '处理冲突'; or when the merge-agent is invoked."
---

# Batch Merge Flow

This skill codifies how to merge multiple approved workbuddy PRs into `main` safely, with design-intent judgment on conflicts.

## When to use

- A batch of workbuddy issues has completed: their PRs are approved by review-agent and CI is green
- User asks to merge a list of PRs
- **Typical invocation is manual** — a human decides a batch is ready and hands the PR list to the merge-agent. The operator may *detect* a batch-ready condition and surface it, but the decision to merge belongs to a human because this skill involves design-intent judgment that operator autonomy shouldn't override.

## Input contract

The caller provides:

```yaml
prs: [#101, #102, #104]      # PR numbers to merge (in no particular order)
repo: Lincyaw/workbuddy       # target repo
base: main                    # base branch (default: main)
mode: auto | review-required  # auto = proceed on safe resolutions; review-required = escalate all conflicts
```

## Procedure

### Phase 0 — Batch coherence analysis

**Before touching any branch**, analyze the batch as a whole. This is where design-intent reasoning happens — do it once upfront, not hunk-by-hunk during rebase.

#### 0.1 Issue-level duplicate detection

For each PR, fetch its linked issue:

```bash
gh pr view <n> --json body,closingIssuesReferences
gh issue view <issue-num> --json title,body,labels
```

Compare issues pairwise. Duplicate signals:

- **Title near-match**: same key terms, similar phrasing
- **Symptom overlap**: both describe the same user-facing problem or error signature
- **AC overlap**: acceptance criteria describe the same behavior
- **File overlap**: both PRs modify the same files in the same region
- **Root-cause overlap**: both diagnose the same underlying cause

For suspected duplicates, consult `decisions.md` and `project-index.yaml` to see if one is actually a re-filing of an older issue or a subset of a larger epic.

Output: a list of **duplicate groups**. Example:

```
group A: #87 ("heartbeat lease bug") + #88 ("worker submit failure")
  — #88's root cause IS #87. Duplicates.
group B: #91 (dynamic config reload) — standalone
```

#### 0.2 PR-level feature overlap detection

For each pair of PRs (within AND across duplicate groups), analyze diffs:

```bash
gh pr diff <n>
```

Overlap signals:

- **Same files changed** with overlapping line ranges
- **Same symbol modified** (function, type, const) — `git diff --name-only`, then grep for symbol names
- **Same behavior implemented** (even if in different files) — e.g., both add a retry loop around the same API call
- **Contradictory changes** — one PR adds X, another removes X

Classify each overlap:

| Class | Example | Default resolution |
|-------|---------|-------------------|
| **Identical intent, one better** | Both fix the same bug; PR A is 3 lines, PR B is 50 lines with refactor | **Adopt PR A entirely, close PR B** |
| **Identical intent, equivalent quality** | Both fix same bug with equally clean code | **Adopt oldest-approved PR, close the other** |
| **Complementary** | Both add distinct features to same file; changes don't semantically conflict | **Merge both in order** |
| **Partially overlapping** | PR A adds features X+Y; PR B adds Y+Z; both need Y | **Cherry-pick** the non-overlapping parts into a merge order: take A's X, take one side's Y, take B's Z |
| **Contradictory design** | PR A uses pattern P1; PR B uses pattern P2; incompatible architecturally | **Escalate** — human must pick the approach |

#### 0.3 Decision authority

For each non-trivial overlap, the skill prescribes:

- **"Adopt one" decisions** (classes 1, 2): the merge-agent decides based on: code quality heuristics (lines changed, test coverage delta, complexity) + design-intent anchors (CLAUDE.md conventions, north-star alignment). Record reasoning.
- **"Cherry-pick" decisions** (class 4): the merge-agent proposes the split, but if the split requires non-trivial code rearrangement (not just a git cherry-pick -x of individual commits), **escalate to human** — don't rewrite authors' commits.
- **Contradictory design** (class 5): always escalate.

Record all decisions in the report (Phase 6) with rationale. For "close PR B" decisions, post a comment on the closing PR explaining why, linking to the kept PR, and thanking the author.

#### 0.4 Output of Phase 0

A revised merge plan:

```yaml
plan:
  merge:
    - pr: 101       # after cherry-pick: take only files A.go, B.go
      cherry_pick_from: [abc123, def456]
      reason: "partial overlap with #102; keeping non-overlapping commits"
    - pr: 102       # full merge
      reason: "standalone"
  close:
    - pr: 105
      reason: "duplicate of #101 with inferior implementation; fewer tests, larger diff"
      comment_to_post: "..."
  escalate:
    - prs: [103, 104]
      reason: "contradictory design choices for internal/store package layout"
      question_for_human: "..."
```

If any item lands in `escalate:` with a **class 5 (contradictory design)**, **stop and surface the question to the invoker before touching any branch**. Resolving design-level conflicts is a human decision.

If only `merge:` and `close:` items remain, proceed to Phase 1 with the revised plan.

### Phase 1 — Preconditions (per PR)

For each PR in `prs`, verify:

1. **Approved**: `gh pr view <n> --json reviewDecision` returns `APPROVED`
2. **CI green**: `gh pr checks <n>` shows all required checks passing
3. **Base is current**: PR's base branch is `main` (no stale PRs targeting old branches)
4. **No `status:blocked` label on linked issue**
5. **Author branch exists** and is not force-pushed recently (check commit SHA hasn't changed since approval timestamp)

Skip PRs that fail any precondition. Record reason in the report.

### Phase 2 — Ordering

Determine merge order using a topological sort:

1. **Explicit dependencies**: parse PR bodies and linked issue bodies for "Blocked by #X" / "Depends on #X". PRs with unresolved deps go after their deps.
2. **File-overlap heuristic**: PRs that touch the same files are ordered by `created_at` (oldest first) so the second PR rebases onto the first.
3. **Disjoint PRs**: no specific order, merge in `created_at` order for reproducibility.

Output a linear order. Cycles (A blocks B and B blocks A) are an error → escalate.

### Phase 3 — Serial merge

For each PR in order:

```bash
# 1. Fetch latest main
git fetch origin main
git checkout main
git pull --ff-only origin main

# 2. Try to rebase PR branch onto main (without pushing)
git fetch origin pull/<n>/head:pr-<n>-rebase
git checkout pr-<n>-rebase
git rebase main
```

**If rebase succeeds cleanly** → proceed to Phase 4 validation, then `gh pr merge <n> --squash --rebase --delete-branch`.

**If rebase has conflicts** → enter Phase 4 conflict resolution on the rebase, then:
- If resolved automatically: push the rebased branch (`git push -f origin pr-<n>-rebase:<original-branch>`), wait for CI, then merge.
- If escalated: abort rebase (`git rebase --abort`), comment on the PR with the conflict report, skip to next PR.

### Phase 4 — Conflict resolution (when rebase conflicts)

Classify each conflict hunk:

| Class | Signature | Strategy |
|-------|-----------|----------|
| **Trivial** | Whitespace, import order, comment-only diffs | Prefer the PR's side; re-run `goimports` or equivalent formatter |
| **Additive** | Both sides add distinct code to the same region (e.g., both add a new case to a switch) | Merge both additions, preserve order by PR created_at |
| **Same-intent** | Both PRs do the same thing slightly differently (e.g., both fix the same bug) | Prefer the approach that aligns with `CLAUDE.md` conventions / `decisions.md` entries. Flag if unclear. |
| **Competing-intent** | PRs implement overlapping features with incompatible designs | **Escalate**. Do not silently pick one. |
| **Structural** | Conflicts span multiple files and involve architectural choices (new package layout, interface changes) | **Escalate**. |

#### Design-intent consultation

Before resolving `same-intent` or `competing-intent` hunks, consult in this order:

1. **`CLAUDE.md`** — project conventions (e.g., "squash + rebase only", "2-agent first principle")
2. **`decisions.md`** — recent architectural decisions with rationale
3. **`project-index.yaml`** — requirement text for both PRs' linked issues; the resolution must satisfy both AC sets
4. **North-star targets** in `CLAUDE.md` — if two resolutions are plausible, pick the one that advances primary targets (requirements coverage, test coverage, build health)

Record your reasoning inline in the resolution commit message:

```
resolve: PR #101 vs PR #102 conflict in cmd/coordinator.go

Context: both PRs modify handleTaskResult. #101 adds tokenized auth check,
#102 adds metrics emission. Changes are orthogonal — merged both.
Design-intent basis: north-star reliability favors both; no competition.
```

### Phase 5 — Post-merge validation

After each successful merge (before proceeding to next PR):

```bash
git checkout main
git pull --ff-only origin main
go build ./...
go vet ./...
go test ./... -count=1
```

**If validation fails**:
- Immediate action: `git revert <merge-sha>` and push, restoring main to green state
- Comment on the just-merged PR explaining the revert with the failing output
- Mark the PR's linked issue with `status:developing` to route back through dev-agent
- Continue to next PR in batch (do not abort the entire batch)

### Phase 6 — Report

Output a structured report:

```markdown
## Merge Batch Report — <timestamp>

### Merged (N)
- #101 — clean rebase
- #102 — additive conflict auto-resolved (switch case)
- #104 — trivial conflict (imports)

### Reverted (M)
- #103 — merged then reverted: go test failed in internal/store
  (revert SHA: abc123; issue re-opened for developing)

### Escalated / Skipped (K)
- #105 — competing-intent conflict with #101 in internal/launcher/permissions.go:
  #101 uses env-var token only; #105 adds keyring fallback.
  Design intent unclear — human decision needed. PR left open.

### Validation
- Final main SHA: def456
- go build: ok
- go test: ok (42 packages, 0 failures)
- go vet: ok
```

## Constraints

- **Never force-push to main.** Always use `gh pr merge --squash --rebase`.
- **Never skip Phase 5 validation.** Each merge is validated before the next begins.
- **Never silently resolve competing-intent conflicts.** Escalate and leave the PR open.
- **Never delete a PR's source branch** unless `--delete-branch` was specified by GitHub merge (which is fine).
- **Never amend merged commits.** If something's wrong, revert + new PR.
- **Never bypass review.** If a PR somehow lost its approval (e.g., new commits pushed), re-trigger review-agent instead of merging.

## Edge cases

### All PRs fail preconditions
Report and exit. Do not attempt to salvage.

### Main drifts during batch (new commit lands from outside)
Re-rebase remaining PRs against new main. This is safe because `main` is always green (Phase 5 invariant).

### Revert chain
If merging PR A causes failure, then reverting A also breaks (highly unusual):
- Reset batch: do not continue
- Alert: "main is broken in a way the batch flow cannot recover from" → escalate to human immediately

### PR branch deleted mid-merge
The original author may have deleted their branch. Fetch via `refs/pull/<n>/head` (always available on GitHub).

## Success criteria

A batch merge succeeded if:
- Every PR in `prs` is either: merged, reverted-and-re-queued, or escalated-with-report
- `main` is in a green state (build + test + vet pass) at the end
- The report accurately reflects all outcomes
- No manual intervention was required for same-intent or trivial conflicts
