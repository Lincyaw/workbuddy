# `internal/failpoints`

Named, runtime-armable fault-injection points for workbuddy. Stdlib-only, opt-in
via build tag, zero cost in production builds.

## Building

Default build — every entry point is a no-op the inliner can fold away:

```bash
go build ./...
```

Inject build — failpoints become real:

```bash
go build -tags faultinject ./...
go build -tags faultinject -o workbuddy .
```

> **WARNING**: production binaries MUST NOT be built with `-tags faultinject`.
> The tag enables an env-var-driven panic/error injection surface that has no
> business in a live coordinator or worker. Treat the tag like `-race`: useful
> in tests, never in a release.

## Hook naming convention

Use `package.function.point`, e.g.

- `poller.list_issues.before`
- `dispatcher.assign_worker.after_db_write`
- `reporter.post_comment.before_gh_call`

The dot-separated form keeps grep simple and lines up with the dispatcher
event names already used elsewhere in workbuddy.

## Env-var grammar

When built with `-tags faultinject`, the package parses `WORKBUDDY_FAILPOINTS`
at `init`. Multiple entries are separated by `;`. Each entry is

```
name=kind(args...)
```

where `kind` is one of `error`, `delay`, `panic`, `return`, `drop`. The first
positional argument is the kind-specific payload:

| Kind     | Positional arg                | Hit behaviour                                |
| -------- | ----------------------------- | -------------------------------------------- |
| `error`  | error message                 | returns `errors.New(msg)`                    |
| `delay`  | `time.ParseDuration` string   | `time.Sleep(d)`, returns nil                 |
| `panic`  | panic message                 | `panic(msg)`                                 |
| `return` | (none)                        | returns `ErrFailpointReturn` sentinel        |
| `drop`   | (none)                        | returns `ErrFailpointDrop` sentinel — caller is expected to silently skip the side-effect (e.g. omit a heartbeat frame) |

Any of the following suffix tokens may appear in the arg list, comma-separated:

- `once` — fire exactly once, then disarm automatically
- `repo=owner/name` — narrow to callers passing `WithRepo("owner/name")`
- `issue=N` — narrow to callers passing `WithIssue(N)`

Malformed entries are reported to stderr at init and skipped; the process
keeps running.

### Examples

```bash
# Inject a one-shot error into the issue lister, only for issue #54 of acme/x.
WORKBUDDY_FAILPOINTS='poller.list_issues.before=error(simulated outage,once,repo=acme/x,issue=54)' \
  ./workbuddy coordinator

# Make every dispatch sleep 2s to observe queue back-pressure.
WORKBUDDY_FAILPOINTS='dispatcher.assign_worker.before=delay(2s)' ./workbuddy serve
```

## Programmatic arming

For tests and integration scenarios, use the Go API directly:

```go
failpoints.Arm("poller.list_issues.before", failpoints.Effect{
    Kind: "error",
    Err:  "simulated gh outage",
    Once: true,
})
defer failpoints.Disarm("poller.list_issues.before")
```

At the hook site:

```go
if err := failpoints.Hit("poller.list_issues.before",
    failpoints.WithRepo(repo), failpoints.WithIssue(issueNum)); err != nil {
    return err
}
```

In the default build the `Hit` call compiles to `return nil` and disappears
from the hot path.

## Scope

This package provides the foundation. The current instrumentation inventory
lands the six fault-simulation hooks that issue #345 identified as the
postmortem-critical decision points for silent-stall reproduction. They are:

| Name                          | File                                | Effect kinds                                    |
| ----------------------------- | ----------------------------------- | ----------------------------------------------- |
| `poller.list_issues.before`   | `internal/poller/poller.go`         | `error` (simulate `gh` outage / auth fail)      |
| `poller.list_issues.after`    | `internal/poller/poller.go`         | `delay` only (simulate slow `gh` response — error/panic/return effects are silently discarded; see hook-site comment) |
| `wstunnel.send.drop`          | `internal/wstunnel/tunnel.go`       | `drop` (silently no-op any outbound frame sent through `Endpoint.send`, including heartbeats) |
| `worker.claim_task.before`    | `internal/app/coordinator.go`       | `error` (simulate transient claim conflict — **WARNING**: arming with a persistent error blocks every worker claim and effectively halts dispatch; pair with `once` or a `repo=`/`issue=` predicate in tests) |
| `worker.agent_exec.die_mid`   | `internal/supervisor/supervisor.go` | `error` (simulate the supervisor crashing AFTER `cmd.Wait` returns but BEFORE the exit bookkeeping runs — `updateAgentExit`/`markExited` are skipped, so the `agents` row stays `running` with no exit code) |
| `store.insert_event.busy`     | `internal/store/store.go`           | `error("SQLITE_BUSY")` (fires inside the busy-retry loop body — combine with `once` to assert one retry; persistent error armings exhaust the four-iteration loop and surface the final wrapped error) |

All six are zero-cost no-ops in the default build (the no-op `Hit` returns
nil unconditionally). They activate only under `-tags faultinject` and are
arming-driven, so a binary built with the tag but no `WORKBUDDY_FAILPOINTS`
env var and no programmatic `Arm` still behaves identically to a production
binary.

The #345 silent-stall regression test (`internal/app/silent_stall_regression_test.go`)
arms `worker.agent_exec.die_mid` plus an aged heartbeat to reproduce the
exact failure mode Wave 2 (REQ-150/151/152) was designed to recover from,
and asserts that the TaskReaper and periodic-recovery sweep together close
the loop.
