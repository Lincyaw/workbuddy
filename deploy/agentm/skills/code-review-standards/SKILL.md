---
name: code-review-standards
description: >
  How to review code against a spec with structured per-AC verdicts.
  Use when reviewing a PR, verifying code fulfills acceptance criteria,
  or selecting between multiple parallel implementations. Trigger on:
  code review, review standards, PR review, 代码审查, 代码评审,
  review agent, per-AC verdict, structured review.
---

# Code Review Standards

Review verifies two things: the code fulfills the spec, and the code
is maintainable. In that order — correct but messy beats clean but wrong.

## Review process

### Step 1: Run the tests

Before reading any code, run the test suite (specifically the tests
generated from the spec):

```bash
pytest tests/test_<feature>.py -v
```

Record per-test pass/fail. This is the ground truth — everything
else in the review is judgment layered on top.

If tests don't exist (test-gen agent didn't run, or spec didn't
have ACs), flag this immediately: review cannot verify correctness
without tests. Send back to the developing state.

### Step 2: Per-AC verdict

For each acceptance criterion, produce a structured verdict:

| AC | Test Result | Code Inspection | Verdict |
|----|-------------|-----------------|---------|
| AC-1 | test_ac1 PASS | Implementation matches spec interface | **PASS** |
| AC-2 | test_ac2 PASS | Edge case handled correctly | **PASS** |
| AC-3 | test_ac3 FAIL | Time handling uses `time.time()` not injected clock | **FAIL** |
| AC-4 | test_ac4 PASS | Reset clears counter dict | **PASS** |
| AC-5 | (no test) | Concurrent access not addressed | **CANNOT JUDGE** |

Three verdicts:
- **PASS**: test passes AND code inspection confirms the AC is met
  (not just the test — the test might have gaps)
- **FAIL**: test fails, OR test passes but code inspection reveals
  the implementation doesn't actually satisfy the AC (test gap)
- **CANNOT JUDGE**: no test exists for this AC, or the AC requires
  manual verification (e.g., UI appearance)

### Step 3: Code quality findings

After the per-AC verdict, note code quality issues. Categorize by
severity:

**Critical** — must fix before merge:
- Security vulnerabilities (injection, auth bypass, secret exposure)
- Data corruption risks (race conditions on shared state, missing
  transactions)
- Correctness bugs not caught by existing tests

**Major** — should fix, but doesn't block merge if ACs pass:
- Missing error handling for documented error cases
- Performance issues in hot paths (unbounded allocations, O(n²)
  where O(n) is straightforward)
- Breaking changes to existing public interfaces without migration

**Minor** — nice to fix, never blocks merge:
- Naming inconsistencies with project conventions
- Missing docstrings on public functions
- Unused imports or variables

### Step 4: Verdict decision

- **All ACs PASS, no critical findings** → approve, transition
  to done
- **Any AC FAIL** → reject, transition to developing. The verdict
  comment must list every failing AC with:
  - What the AC requires
  - What the implementation does instead
  - The specific test failure message (if available)
  - A suggestion for how to fix (optional but helpful)
- **CANNOT JUDGE on any AC** → flag but don't block if all
  testable ACs pass. Note what couldn't be verified and why.

## Selecting between parallel implementations

When multiple dev sessions produce implementations that all pass
tests, rank by:

1. **Correctness** — any AC FAIL disqualifies
2. **Simplicity** — fewer lines of code, fewer abstractions, fewer
   dependencies. The implementation that a new contributor would
   understand fastest.
3. **Idiomaticity** — follows the project's existing patterns.
   If the codebase uses dataclasses, don't introduce attrs. If it
   uses `fmt.Errorf`, don't introduce a custom error package.
4. **Test coverage beyond spec** — did the dev agent add extra
   tests for edge cases not in the ACs? Bonus, not required.
5. **Performance** — only as a tiebreaker, and only if measured.
   "Looks faster" is not a valid criterion.

When selecting, name the winner and briefly explain why:

```
Selected: rollout-2
Reason: All 7 ACs pass. Rollout-2 implements the rate limiter as
a single function with a dict + timestamp, while rollout-1 uses a
class hierarchy with 3 abstract base classes. Rollout-2 is 40 lines
vs 120 lines with identical behavior. Rollout-3 fails AC-3.
```

## Structured verdict format

The review comment MUST follow this format so the orchestrator can
parse it and provide structured feedback to the dev agent:

```markdown
<!-- workbuddy:review-verdict -->
## Review Verdict

### Test Results
- Total: 7 | Pass: 5 | Fail: 2
- Failing: test_ac3_window_expiry, test_ac6_zero_max_requests

### Per-AC Verdict
| AC | Verdict | Evidence |
|----|---------|----------|
| AC-1 | PASS | test_ac1 passes; implementation uses counter dict |
| AC-2 | PASS | test_ac2 passes; boundary check correct |
| AC-3 | FAIL | test_ac3 fails: time.time() not mockable, flaky |
| AC-4 | PASS | test_ac4 passes; reset clears entry |
| AC-5 | PASS | test_ac5 passes; separate dict entries per client |
| AC-6 | FAIL | test_ac6 fails: ValueError not raised for 0 |
| AC-7 | PASS | test_ac7 passes; ValueError raised for negative |

### Code Quality
- [critical] AC-6: constructor accepts max_requests=0 without
  raising ValueError (missing validation)
- [minor] rate_limiter.py: unused import `collections`

### Decision
REJECTED — 2 ACs fail. See failing ACs above for required fixes.
```

The `<!-- workbuddy:review-verdict -->` marker is required on the
first line. The orchestrator uses it to locate the most recent
verdict when assembling the next dev cycle's prompt.

## Common review mistakes

**Reviewing style when correctness is unclear.** Don't bikeshed
variable names while tests are failing. Fix correctness first.

**Trusting tests blindly.** Tests pass ≠ implementation is correct.
Tests can have gaps. A test that checks `assert result is not None`
passes even if the result is wrong. Read the code.

**Rejecting for style alone.** If all ACs pass and there are no
critical findings, the implementation is acceptable. Style issues
are minor findings, not rejection reasons.

**Vague rejection.** "Needs work" is not actionable. The dev agent
needs: which AC fails, what error message, what's wrong with the
implementation, what would fix it.

**Reviewing without test results.** If you haven't run the tests,
you're guessing at correctness. Run them first. Always.
