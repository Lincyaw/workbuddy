# `hack/e2e/` — local kind smoke for workbuddy v0.6 K8s path

REQ-147 / issue [#335](https://github.com/Lincyaw/workbuddy/issues/335). Per
decision doc [`docs/decisions/2026-05-13-k8s-agentm-otel.md`](../../docs/decisions/2026-05-13-k8s-agentm-otel.md)
§ Block 5, the K8s deployment path needs an end-to-end smoke
validation. This directory holds that.

> **Local-only.** `make e2e` is not wired into CI. It is meant to be run
> on a developer machine before cutting a v0.6.x tag. Provisioning a CI
> runner with docker + kind is explicitly out of scope (issue #335 § Out
> of scope).

## Prerequisites

The smoke shells out to four host tools — install all of them before
running anything in this directory:

| Tool      | Tested version | Install hint                                                  |
|-----------|----------------|---------------------------------------------------------------|
| `docker`  | 24.x+          | https://docs.docker.com/engine/install/                       |
| `kind`    | v0.24+         | `go install sigs.k8s.io/kind@latest`                          |
| `helm`    | v3.14+         | https://helm.sh/docs/intro/install/                           |
| `kubectl` | v1.30+         | `kind` won't ship one for you                                 |

If your dev machine doesn't have docker + kind (e.g. you're reviewing
this PR from a sandbox), run `make dry-validate` instead. That target
only needs `docker` and `helm`, doesn't create a cluster, and validates:

- `helm lint deploy/helm/workbuddy/` passes.
- `helm template` renders the chart with the smoke values without
  error (the rendered manifest is dropped at `/tmp/workbuddy-e2e-rendered.yaml`).
- Both Dockerfiles (`workbuddy-image/`, `agentm-fake/`, `fake-gh/`) build.

## Quick start

From `hack/e2e/`:

```bash
make e2e             # full smoke: spin up, install, assert, tear down.
make e2e KEEP=1      # same, but leave the cluster running on success.
make e2e-up          # just bring everything up; don't tear down.
make e2e-assert      # run the assertion phase against an existing cluster.
make e2e-down        # delete the kind cluster.
make dry-validate    # offline lint/template/build — no cluster needed.
```

On failure during `make e2e`, the `EXIT` trap runs `dump-logs` (pod
logs for coordinator, mysql, otel-collector, fake-gh) and then
`e2e-down`. Override the auto-teardown with `KEEP=1` if you want to
poke at the failing cluster after the trap fires; the dump runs
either way.

## What's actually asserted

The smoke validates **the deployment plane** end-to-end:

1. `kind` cluster comes up.
2. `docker build` produces a workbuddy container with the binary built
   from the current worktree.
3. `kind load docker-image` makes that image available without a
   registry round-trip.
4. The chart's MySQL DSN wiring (`WORKBUDDY_MYSQL_DSN` env →
   `resolveCoordinatorDSN` → `store.New("mysql://...")`) reaches the
   in-cluster MySQL — we verify by `kubectl exec` into the MySQL pod
   and listing the `workbuddy` database tables. If `store.New` had
   silently fallen back to SQLite (the pre-G1 bug, see PR description),
   no tables would exist on the MySQL side and this step fails.
5. The chart's OTel endpoint wiring sends spans to the in-cluster
   debug collector. This is a *soft* assertion — the smoke does not
   fail when no workbuddy span lands in 30s, because span emission
   depends on the coordinator hitting a tracer-instrumented path
   during the smoke window (see Known limitations below).
6. The fake-gh server is reachable from inside the cluster.

## Known limitations

- **AgentM dispatch is not exercised end-to-end.** The issue #335 AC
  ("AgentM pod runs; PR creation call hits the fake-gh server; label
  flip hits the fake-gh server") requires the coordinator's poller to
  ingest a fake issue, dispatch it to an AgentM agent, run the fake
  binary container, and have the coordinator-side gitops adapter
  commit + open a PR against the fake-gh server.

  The `fake-gh` server in `fake-gh/server.py` stubs `rate_limit`,
  the GraphQL viewer query, label edits, comments, and PR creates —
  enough to *record* the calls — but it does not faithfully emulate
  the full gh-CLI JSON contract the poller relies on to discover and
  classify issues. Closing that gap is a separate follow-up; the
  Makefile's `e2e-assert` target prints a "SKIPPED" banner for this
  assertion with the reason.

- **OTel span assertion is soft, not hard.** As of v0.6 the coordinator
  emits spans on dispatch / poll cycles, not just on startup. Without
  the AgentM dispatch path above being exercised, the only spans that
  land are from the poll loop, which may or may not have ticked by the
  30s deadline. Promoting this to a hard assertion lands together with
  the dispatch path.

- **No CI.** `make e2e` requires docker-in-docker (or a privileged
  runner with kind support) and a couple of minutes per run. The cost
  is not justified for a smoke that is gated to local pre-release
  validation. Issue #335 explicitly states "Does NOT run in CI."

## Files

```
hack/e2e/
├── Makefile                       # entry points (e2e, e2e-up, dry-validate, …)
├── kind-config.yaml               # single-node kind cluster spec
├── README.md                      # you are here
├── values/
│   └── workbuddy-values.yaml      # helm -f override for the smoke topology
├── manifests/
│   ├── mysql.yaml                 # bare mysql:8.0 Deployment + Service
│   ├── otel-collector.yaml        # otel/opentelemetry-collector-contrib + debug exporter
│   └── fake-gh.yaml               # fake-gh Deployment + Service
├── workbuddy-image/
│   └── Dockerfile                 # build the workbuddy binary into a container
├── agentm-fake/
│   ├── Dockerfile                 # baker for the fake AgentM dev container
│   └── agentm                     # POSIX-sh fake AgentM, RESULT-line + artifact
└── fake-gh/
    ├── Dockerfile                 # stdlib-Python image
    └── server.py                  # records every request on /_assertions
```

## Coordinator MySQL wiring (G1 prerequisite)

This smoke depends on the coordinator honoring `WORKBUDDY_MYSQL_DSN`.
That wiring landed in the same commit as this scaffold — see
`cmd/coordinator.go` (`resolveCoordinatorDSN` + `store.NewCoordinator`)
and the unit test at `cmd/coordinator_dsn_test.go`. Without it, the
chart's `mysql.dsn` value would be silently ignored and the
coordinator pod would create a SQLite file at the default path on the
pod's ephemeral filesystem — losing all state on restart.
