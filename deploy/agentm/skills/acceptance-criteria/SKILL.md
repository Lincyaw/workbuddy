---
name: acceptance-criteria
description: >
  Reference for writing and evaluating acceptance criteria. Use when
  an AC is flagged as untestable, when the spec-review agent needs
  to judge testability, or when deriving ACs from a vague requirement.
  Trigger on: acceptance criteria, AC, testability, 验收标准,
  how to write AC, is this AC testable, untestable criterion.
---

# Acceptance Criteria Reference

An acceptance criterion (AC) is a single, verifiable condition that
must hold for the implementation to be considered correct. ACs are
the bridge between human requirements and automated tests.

## The testability test

An AC is testable if you can answer YES to all three:

1. **Can you write the test function signature right now?**
   If you can write `def test_ac3_window_expiry():` and know what
   it should call and assert, the AC is testable.

2. **Is the pass/fail deterministic?**
   Running the test twice with the same input produces the same
   result. No randomness, no timing sensitivity (without clock
   injection), no dependency on external services (without mocking).

3. **Does it reference concrete values?**
   Names from the Interface section, specific inputs, specific
   expected outputs or error types. Not "the function" but
   "`parse_config`". Not "an error" but "`FileNotFoundError`".

## Patterns

### Functional behavior
```
AC-N: <function>(<specific input>) returns <specific output>
AC-N: <function>(<invalid input>) raises <specific error type>
```

### State transitions
```
AC-N: After calling <method A>, calling <method B> returns <value>
AC-N: <object> initialized with <params> has <property> = <value>
```

### Performance (when in scope)
```
AC-N: <operation> on <N items> completes in < <threshold>
AC-N: Memory usage does not exceed <limit> for <workload>
```
Always specify the workload size and the threshold.

### API contracts
```
AC-N: POST /path with <body> returns <status code> and <response shape>
AC-N: GET /path without auth returns 401
```

### Data invariants
```
AC-N: After <operation>, <invariant> holds (e.g., balance >= 0)
AC-N: <collection> never contains duplicate <key field>
```

## Common untestable patterns and fixes

| Untestable AC | Why | Fix |
|---|---|---|
| "Should be fast" | No threshold | "Completes in < 200ms for N=1000" |
| "Handles errors gracefully" | "Gracefully" is subjective | "Raises ValueError with message containing the invalid field name" |
| "Works correctly" | Tautological | Split into specific input/output pairs |
| "Is well-documented" | Subjective | "Every public function has a docstring with parameter descriptions" (checkable by AST) |
| "Is secure" | Too broad | Split: "Escapes HTML in user input" + "Rejects SQL in query params" + "Rate-limits login attempts" |
| "Improves performance" | No baseline or target | "p99 latency drops from > 500ms (current) to < 200ms" |
| "Supports all edge cases" | "All" is unbounded | Enumerate: empty input, null, max int, Unicode, concurrent |
| "Is backward compatible" | Unstated contract | "Existing callers of `old_function(a, b)` continue to work without changes" |

## Deriving ACs from vague requirements

When the issue says "add caching to the API":

1. **Identify the interface**: what function/endpoint gets cached?
   → "GET /api/users/:id responses are cached"

2. **Define the observable behavior**: what changes for the caller?
   → "Second request for the same user_id returns in < 5ms"
   → "Response includes `X-Cache: HIT` header"

3. **Define the boundaries**: when does the cache invalidate?
   → "PUT /api/users/:id invalidates the cached entry"
   → "Cache entries expire after 300 seconds"

4. **Define the error cases**: what if cache is unavailable?
   → "If Redis is unreachable, requests fall through to the database
     (no error returned to caller)"

5. **State assumptions**: "Assuming cache backend is Redis. If
   in-memory cache is preferred, AC-4 changes."

Result:
```
AC-1: GET /api/users/:id returns identical response on second call
AC-2: Second call completes in < 5ms (first call unconstrained)
AC-3: Response includes X-Cache header (HIT or MISS)
AC-4: PUT /api/users/:id causes next GET to return updated data
AC-5: Cache entries not accessed for 300s are evicted
AC-6: If Redis is unreachable, GET still returns correct data from DB
```

## AC granularity

**Too coarse**: "AC-1: The rate limiter works correctly for all
cases." This is untestable and will produce a test that either
tests one case (incomplete) or a monster test that's hard to
debug when it fails.

**Too fine**: "AC-1: The constructor stores max_requests. AC-2:
The constructor stores window_seconds." This tests implementation
details, not behavior. The implementation could store them
differently and still be correct.

**Right level**: One observable behavior per AC. "AC-1: First N
requests pass. AC-2: Request N+1 is rejected." These test what
the caller sees, not how the internals work.

## Numbering and traceability

- Number sequentially: AC-1, AC-2, AC-3...
- Don't renumber when removing an AC — skip the number. This
  preserves traceability to tests named `test_ac3_...`.
- When adding ACs during review feedback, continue from the
  last number.
- The test-to-AC mapping (produced by test-gen agent) is the
  traceability record.
