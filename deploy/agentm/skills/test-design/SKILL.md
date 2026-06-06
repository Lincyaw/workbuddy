---
name: test-design
description: >
  How to derive tests from a spec. Covers extracting test cases from
  Use Case extensions, GWT scenarios, and Design by Contract invariants.
  Core insight: the test oracle is an invariant — tests find their
  value in checking exception branches and properties that must always
  hold, not in re-stating the happy path. Trigger on: test generation,
  write tests, test design, integration test, 写测试, 测试设计,
  test from spec, test oracle, invariant, property test, 不变量.
---

# Test Design Guide

Tests are derived from the spec, not from the code. The test-gen
agent writes tests without seeing any implementation.

The fundamental insight: **a test oracle is an invariant.** Every
useful test checks that some property holds (or is violated in a
specific way). The happy path is usually obvious and low-value — the
real testing power is in exception branches (what should NOT happen)
and invariants (what should ALWAYS hold).

## What to test, ordered by value

### 1. Exception branches (highest value)

Every Use Case extension and every error-raising precondition is an
exception branch. These are where bugs hide — the happy path gets
exercised by every demo and manual test, but the `*a. Database
unavailable at any step` extension only fires in production at 3am.

From a Use Case:
```
Extension 2a: Scope not in allowed set → 400
Extension 5a: Key quota exhausted → 429
Extension *a: Database unavailable → 503, no partial state
```

Each becomes a test:
```python
def test_ext_2a_invalid_scope_rejected():
    response = create_api_key(scopes=["admin"])
    assert response.status == 400
    assert "invalid scope" in response.body["error"]

def test_ext_5a_quota_exhausted():
    for _ in range(10):
        create_api_key(scopes=["read"])
    response = create_api_key(scopes=["read"])
    assert response.status == 429

def test_ext_star_a_db_unavailable_no_partial_state():
    with mock_db_failure():
        response = create_api_key(scopes=["read"])
    assert response.status == 503
    assert count_keys_in_db() == 0  # no partial state
```

### 2. Invariants (highest leverage)

A Contract invariant holds after EVERY public method call. Testing
it once is nice; testing it across random operation sequences is
powerful — this is property-based testing.

From a Contract:
```
Invariants:
- total_value = sum(order.price * order.qty for order in orders)
- All orders have unique IDs
- No order has qty <= 0
```

The invariant test:
```python
from hypothesis import given, strategies as st

@given(operations=st.lists(st.one_of(
    st.tuples(st.just("add"), st.integers(1, 1000), st.integers(1, 100)),
    st.tuples(st.just("cancel"), st.integers(0, 999)),
)))
def test_invariant_total_value_consistent(operations):
    book = OrderBook()
    ids = []
    for op in operations:
        if op[0] == "add":
            order_id = book.add_order(Order(price=op[1], qty=op[2]))
            ids.append(order_id)
        elif op[0] == "cancel" and ids:
            idx = op[1] % len(ids)
            try:
                book.cancel_order(ids[idx])
                ids.pop(idx)
            except KeyError:
                pass

    # THE INVARIANT — must hold after any sequence of operations
    expected = sum(o.price * o.qty for o in book.orders)
    assert book.get_total_value() == expected
    assert len(set(o.id for o in book.orders)) == len(book.orders)
    assert all(o.qty > 0 for o in book.orders)
```

One invariant test with random operations can find bugs that 50
hand-written happy-path tests miss. The invariant is the oracle —
"this property must always hold" is a stronger statement than "this
specific input produces this specific output."

### 3. Postcondition contracts (medium value)

Each postcondition is a test: call the function with valid inputs,
verify the postcondition holds.

From a Contract:
```
add_order(order):
  postcondition: count increased by 1, total_value increased by
  order.price * order.qty
```

```python
def test_add_order_postcondition():
    book = OrderBook()
    before_count = book.count
    before_value = book.get_total_value()
    order = Order(price=100, qty=2)

    book.add_order(order)

    assert book.count == before_count + 1
    assert book.get_total_value() == before_value + 200
```

### 4. Precondition violations (medium value)

Each precondition that raises should be tested:

```
add_order(order):
  precondition: order.qty > 0
  raises: ValueError: qty <= 0
```

```python
def test_add_order_precondition_zero_qty():
    book = OrderBook()
    with pytest.raises(ValueError):
        book.add_order(Order(price=100, qty=0))

def test_add_order_precondition_negative_qty():
    book = OrderBook()
    with pytest.raises(ValueError):
        book.add_order(Order(price=100, qty=-1))
```

### 5. GWT scenarios (direct translation)

Each Given-When-Then scenario is already a test in prose form.
Translate directly:

```
Scenario: Retry-After header value
  Given client has exceeded their rate limit
  And their window resets in 23 seconds
  When they receive a 429 response
  Then the Retry-After header value is "23"
```

```python
def test_gwt_retry_after_header():
    # Given
    limiter = RateLimiter(max_requests=1, window_seconds=30)
    clock = MockClock(time=0)
    limiter.check("c1", clock=clock)  # exhaust quota
    clock.advance(7)  # 7 seconds elapsed, 23 remaining

    # When
    result = limiter.check("c1", clock=clock)

    # Then
    assert result.allowed is False
    assert result.retry_after == 23
```

### 6. Happy path (lowest value, but don't skip)

The main success scenario of a Use Case. Low value because it's
obvious, but you still need at least one test to verify the basic
flow works:

```python
def test_main_success_create_api_key():
    response = create_api_key(scopes=["read", "write"])
    assert response.status == 201
    assert "secret" in response.body
    assert response.body["scopes"] == ["read", "write"]
```

One test, not ten variations. The exception branches and invariants
are where your test budget should go.

## Extraction rules by spec method

### From Use Case → tests

| Spec element | Test type | Naming |
|---|---|---|
| Main Success Scenario | 1 happy-path integration test | `test_uc_<name>_main_success` |
| Extension Na | 1 test per extension | `test_uc_<name>_ext_<N><letter>_<description>` |
| Postcondition | assert in happy-path test | (included in main success test) |
| `*a` (at any step) | 1 test with fault injection | `test_uc_<name>_ext_star_a_<description>` |

### From GWT → tests

Direct 1:1 mapping. Each scenario is one test:

| Spec element | Test type | Naming |
|---|---|---|
| Scenario | 1 test | `test_gwt_<scenario_name_snake_case>` |
| Given | test setup / fixtures | — |
| When | function call / action | — |
| Then / And | assertions | — |

### From Contract → tests

| Spec element | Test type | Naming |
|---|---|---|
| Invariant | 1 property-based test (hypothesis/rapid) | `test_invariant_<description>` |
| Postcondition | 1 test per method | `test_contract_<method>_postcondition` |
| Precondition violation | 1 test per raises clause | `test_contract_<method>_rejects_<condition>` |

### From plain AC → tests

| Spec element | Test type | Naming |
|---|---|---|
| AC-N | 1 test | `test_ac<N>_<description>` |

## Invariant-first test design

When the spec has Design by Contract sections, start with the
invariants. They produce the highest-value tests.

Steps:
1. Read all invariants from the Contract section
2. For each invariant, write a property-based test that:
   - Creates the object
   - Applies a random sequence of public method calls
   - Asserts the invariant holds after each call (or at the end)
3. Then write the per-method postcondition tests
4. Then write the precondition violation tests
5. Finally, write the GWT scenario tests and Use Case extension tests

This ordering front-loads the tests most likely to find real bugs.

## When you don't have hypothesis/property-testing

Not every language or project has property-based testing. The
invariant-first approach still works — just enumerate representative
sequences manually:

```python
def test_invariant_total_value_after_mixed_operations():
    book = OrderBook()
    book.add_order(Order(price=10, qty=5))   # total: 50
    book.add_order(Order(price=20, qty=3))   # total: 110
    id3 = book.add_order(Order(price=5, qty=10))  # total: 160
    book.cancel_order(id3)                    # total: 110

    assert book.get_total_value() == 110
    assert book.count == 2
    assert all(o.qty > 0 for o in book.orders)
```

Less powerful than random generation, but still tests the invariant
across a non-trivial sequence rather than testing one operation in
isolation.

## Verification: tests must fail without implementation

After writing tests, run them. Every test MUST fail:

- **ImportError** — module doesn't exist yet. Correct.
- **AssertionError** — stub returns wrong values. Correct.
- **AttributeError** — class/function doesn't exist. Correct.

A test that passes without implementation is a bug. Investigate:
- Feature already implemented? Check the codebase.
- Assertion trivially true? (`assert x is not None`)
- Wrong import? (testing an existing function with same name)

## Test-to-spec traceability

After writing tests, produce a mapping showing which spec elements
each test covers:

```
## Test-to-Spec Mapping

| Spec Element | Test | Oracle Type |
|---|---|---|
| UC ext 2a: invalid scope | test_uc_create_key_ext_2a | Exception branch |
| UC ext *a: DB failure | test_uc_create_key_ext_star_a | Exception + invariant (no partial state) |
| Contract invariant: total_value | test_invariant_total_value | Property-based invariant |
| Contract pre: qty > 0 | test_contract_add_order_rejects_zero | Precondition violation |
| GWT: retry-after header | test_gwt_retry_after_header | Behavioral scenario |
| AC-1: config loaded | test_ac1_config_loaded | Direct assertion |
```

The "Oracle Type" column makes explicit what kind of check the test
performs. This helps the review agent assess coverage quality — a
test suite with only "Direct assertion" oracles and no invariant or
exception branch tests is weak regardless of line coverage numbers.

## Anti-patterns

**Testing the happy path N times.** Three tests that call `add_order`
with slightly different prices all test the same code path. Write one
happy-path test and spend the budget on exception branches.

**Testing implementation details.** "Assert internal `_counter` dict
has key 'c1'" — this breaks when the implementation changes data
structures. Test the public postcondition: "check('c1') returns False."

**Invariant as a one-shot.** Writing `assert total == 50` after one
operation tests a specific case, not the invariant. Apply a sequence
of operations and check the invariant holds across all of them.

**Mocking the thing under test.** Mock the clock, the database, the
network — not the class you're testing.

**time.sleep in tests.** Flaky, slow. Inject a mock clock.

**Tests that depend on execution order.** Each test must be
independently runnable. Use fresh fixtures.
