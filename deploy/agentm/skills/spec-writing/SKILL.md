---
name: spec-writing
description: >
  How to write a design spec that downstream agents can implement and
  test against. Covers four composable methods: Use Case for interaction
  flows, Given-When-Then for behavioral scenarios, Design by Contract
  for function-level guarantees, and plain AC for simple tasks. Use when
  writing a spec, reviewing spec completeness, or when a dev/test-gen
  agent reports the spec is too vague. Trigger on: spec, 设计文档,
  需求文档, 接口设计, write spec, design doc, acceptance criteria,
  use case, given when then, design by contract, 契约, 用例.
---

# Spec Writing Guide

A spec is the single source of truth for what gets built. Tests are
derived from it. Code is implemented against it. Review verifies
conformance to it.

This guide covers four composable spec methods. They are not
alternatives — most real tasks use two or three together. The choice
depends on what kind of thing you're specifying.

## Choosing and combining methods

| What you're specifying | Primary method | Often combined with |
|---|---|---|
| Multi-step interaction (registration, payment, approval flow) | **Use Case** | GWT for key scenarios, Contract for internal functions |
| API endpoint / event handler / state transition | **Given-When-Then** | Contract for the functions behind the API |
| Library function / data structure / algorithm | **Design by Contract** | GWT for integration scenarios |
| Simple bug fix / config change / one-liner | **Plain AC** | — |
| New module (API + internal logic + data model) | **All three combined** | Use Case at system level, GWT at API level, Contract at function level |

These methods describe the SAME system at different granularities.
Use Case describes the flow between actors. GWT describes observable
behavior at the API boundary. Contract describes the guarantees of
each function. They nest naturally:

```
Use Case: "User submits order"
  ├── Step 3: "system validates inventory"
  │     └── GWT scenario: Given item in stock, When order submitted, Then stock decremented
  │           └── Contract: decrement_stock(item_id, qty)
  │                 precondition:  qty > 0, item exists
  │                 postcondition: stock[item_id] decreased by qty
  │                 invariant:     stock[item_id] >= 0
  ├── Extension 3a: "item out of stock"
  │     └── GWT scenario: Given item stock = 0, When order submitted, Then 409 returned
  ...
```

## Method 1: Use Case

Best for: multi-step flows involving actors (users, systems,
external services). The main value is systematic enumeration of
exception branches — each Extension is a test case.

### Template

```markdown
## Use Case: <Name>

**Actor**: <who initiates>
**Preconditions**: <what must be true before this flow starts>

### Main Success Scenario
1. <Actor> does <action>
2. System does <response>
3. <Actor> does <action>
4. System does <response>
...

### Extensions
<step>a. <condition> →
    1. System does <alternative response>
    2. <outcome>
<step>b. <condition> →
    1. System does <alternative response>
    2. <outcome>

**Postconditions**: <what is true after successful completion>
```

### Example

```markdown
## Use Case: Create API Key

**Actor**: Authenticated admin user
**Preconditions**: User has admin role, API key quota not exhausted

### Main Success Scenario
1. Admin sends POST /api/keys with { "name": "prod-key", "scopes": ["read", "write"] }
2. System validates scopes against allowed set
3. System generates a 256-bit random key
4. System stores key hash (not plaintext) with metadata
5. System returns 201 { "id": "key_xxx", "secret": "ak_...", "scopes": ["read", "write"] }
6. System logs key creation event

### Extensions
2a. Scope not in allowed set →
    1. System returns 400 { "error": "invalid scope: 'admin'" }
    2. No key created
2b. Scopes array empty →
    1. System returns 400 { "error": "at least one scope required" }
3a. Random generation fails (entropy exhaustion) →
    1. System returns 503 { "error": "try again" }
    2. No key created, no partial state
5a. Admin has reached key quota (10) →
    1. System returns 429 { "error": "key quota exhausted" }
    2. Checked BEFORE key generation (no wasted entropy)
*a. Database unavailable at any step →
    1. System returns 503
    2. No partial state (transaction rolled back)

**Postconditions**: New key exists in DB (hashed), plaintext returned
exactly once, audit log entry written
```

Each extension (2a, 2b, 3a, 5a, *a) maps directly to a test case.
The asterisk `*a` means "at any step" — this produces a test that
injects a DB failure and verifies no partial state.

### When to skip

If the task has no multi-step flow (e.g., "add a pure function that
parses config files"), Use Case adds nothing. Use Contract instead.

## Method 2: Given-When-Then (GWT)

Best for: describing observable behavior at system boundaries —
API responses, state transitions, event emissions. Each scenario
is a self-contained test case.

### Template

```markdown
## Scenario: <descriptive name>
  Given <initial state / precondition>
  And <additional setup>
  When <action / trigger>
  Then <expected outcome>
  And <additional assertions>
```

### Example

```markdown
## Scenario: Rate limit exceeded
  Given a RateLimiter with max_requests=3, window_seconds=10
  And client "c1" has made 3 requests in the last 5 seconds
  When client "c1" makes a 4th request
  Then check() returns False
  And get_remaining("c1") returns 0

## Scenario: Window expiry restores quota
  Given a RateLimiter with max_requests=3, window_seconds=10
  And client "c1" has exhausted their quota
  When 10 seconds elapse (via injected clock)
  Then check("c1") returns True
  And get_remaining("c1") returns 3

## Scenario: Independent client counters
  Given a RateLimiter with max_requests=1, window_seconds=60
  And client "c1" has exhausted their quota
  When client "c2" calls check()
  Then check("c2") returns True
  And get_remaining("c1") is still 0
```

### When to skip

For internal functions where the contract (pre/post/invariant) is
more precise than a scenario. GWT is great for "from the outside it
looks like..." but awkward for "this internal invariant always holds."

## Method 3: Design by Contract

Best for: library functions, data structures, algorithms. Each method
gets three things: what the caller must guarantee (precondition), what
the method guarantees back (postcondition), and what is always true
(invariant).

### Template

```markdown
## Contract: <Class or Module>

### Invariants
- <property that holds after every public method call>

### <method signature>
- **Precondition**: <what must be true when called>
- **Postcondition**: <what is guaranteed after return>
- **Raises**: <error type>: <when>
```

### Example

```markdown
## Contract: OrderBook

### Invariants
- total_value = sum(order.price * order.qty for order in orders)
- len(orders) == count
- All orders have unique IDs
- No order has qty <= 0

### add_order(order: Order) -> str
- **Precondition**: order.qty > 0, order.price >= 0
- **Postcondition**: returned ID is in orders, count increased by 1,
  total_value increased by order.price * order.qty
- **Raises**: ValueError: qty <= 0 or price < 0
- **Raises**: DuplicateError: order.id already exists

### cancel_order(order_id: str) -> Order
- **Precondition**: order_id exists in orders
- **Postcondition**: order_id no longer in orders, count decreased by 1,
  total_value decreased by cancelled order's value, returned order
  matches the cancelled one
- **Raises**: KeyError: order_id not found

### get_total_value() -> Decimal
- **Precondition**: none
- **Postcondition**: returns current total_value (same as invariant)
```

**Why invariants matter for testing**: the invariant is the most
powerful test oracle. Instead of testing specific inputs and outputs,
you test that the invariant holds after EVERY operation, with random
sequences of operations (property-based testing). If `total_value`
ever diverges from the sum of order values, something is broken —
regardless of which specific sequence of adds and cancels caused it.

### When to skip

For tasks that are purely about wiring (configuration, routing,
deployment) where there's no computation to contract over.

## Method 4: Plain AC

For simple tasks where the other three are overhead. Just numbered
conditions.

```markdown
## Acceptance Criteria

AC-1: Config file `logging.yaml` is loaded at startup
AC-2: If `logging.yaml` is missing, default to INFO level
AC-3: Log level change takes effect without restart (SIGHUP handler)
```

Use when:
- Bug fix with obvious scope
- Config change
- One function, few edge cases
- The task is small enough that a 3-line spec is complete

## Composing methods in practice

A real spec for a new module might look like:

```markdown
# Spec: Rate Limiting Service

## Use Case: API Rate Limiting

**Actor**: API client (authenticated via API key)
**Preconditions**: Client has a valid API key

### Main Success Scenario
1. Client sends request with API key in header
2. System looks up rate limit config for the key's tier
3. System checks current request count against limit
4. Request count is below limit → request proceeds
5. System increments counter, sets/extends TTL

### Extensions
3a. No rate limit config for this tier →
    1. System applies default limit (100 req/min)
4a. Request count >= limit →
    1. System returns 429 with Retry-After header
    2. Counter is NOT incremented (rejected requests don't count)
*a. Redis unavailable →
    1. System allows request (fail-open policy)
    2. System logs warning with client_id and timestamp

## Scenarios (Given-When-Then)

### Scenario: Retry-After header value
  Given client has exceeded their rate limit
  And their window resets in 23 seconds
  When they receive a 429 response
  Then the Retry-After header value is "23"

### Scenario: Tier upgrade takes effect immediately
  Given client "c1" is on tier "free" (100 req/min)
  And client "c1" has made 80 requests this minute
  When client "c1" is upgraded to tier "pro" (1000 req/min)
  Then client "c1"'s next request succeeds (80 < 1000)

## Contracts

### RateLimiter(redis: Redis, config: TierConfig)

#### Invariants
- request_count[client] >= 0 for all clients
- request_count[client] <= tier_limit[client.tier] after any
  successful check() (rejected requests don't increment)

#### check(client_id: str) -> RateLimitResult
- **Precondition**: client_id is non-empty
- **Postcondition (allowed)**: request_count[client_id] incremented
  by 1, returns RateLimitResult(allowed=True, remaining=N)
- **Postcondition (rejected)**: request_count unchanged, returns
  RateLimitResult(allowed=False, retry_after=seconds)
- **Raises**: ValueError: client_id is empty

### Design Notes
- Redis INCR + EXPIRE for atomic counter management
- Fail-open on Redis failure (availability > strictness)
- Counter key format: `rl:{client_id}:{minute_bucket}`
- TTL = window size + 1 second (avoid off-by-one on bucket boundary)
```

This spec uses all three methods at appropriate levels: Use Case for
the overall flow, GWT for specific behavioral scenarios, Contract
for the core function's guarantees.

## Spec completeness checklist

Before marking a spec as ready for review:

```
[ ] Every public function/endpoint has defined input types and
    return types (Interface)
[ ] Every error condition has a named error type and trigger
    condition
[ ] Every Use Case extension is a specific, named exception path
    (not "something goes wrong")
[ ] Every GWT scenario has concrete values (not "some input")
[ ] Every Contract has at least one invariant
[ ] Preconditions are stated (what the caller must guarantee)
[ ] Out-of-scope is explicit (what this spec does NOT cover)
[ ] No subjective criteria ("clean", "fast", "good") without
    quantification
```
