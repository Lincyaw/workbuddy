// Package app owns the runtime-assembly graph for workbuddy: wiring store,
// recovery, security, eventlog, notifier, poller, state machine, router, and
// worker into a coherent topology. The cmd/* layer is intentionally kept to
// flag parsing + config build + a call into this package, so the composition
// boundary no longer lives inside the CLI package.
//
// Entry points:
//
//   - RunServe: single-process "serve" topology (Coordinator + embedded Worker).
//   - RunCoordinator: distributed coordinator HTTP service (poll + route +
//     long-poll task API for remote workers).
//
// Supporting building blocks (PollerManager, NotifierRuntime,
// CoordinatorConfigRuntime, RunningTasks, ClosedIssues, GHCLIReader,
// RecoverTasks, AllowSecurityEvent, LogSecurityPosture) are exported so that
// tests can exercise them without standing up a Cobra command.
package app
