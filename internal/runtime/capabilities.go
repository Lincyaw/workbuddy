package runtime

// Capabilities is a descriptor every Runtime publishes about how it executes
// agents and interacts with the surrounding control loop. It lets
// runtime-agnostic callers (the claim filter, a future runtime-benchmark
// harness, and — today — the capability-driven label-write wiring) reason
// about backends uniformly instead of branching on the runtime name. See
// docs/decisions/2026-06-06-runtime-strategy-and-convergence.md (§1, §3).
type Capabilities struct {
	// Sandboxed reports whether agent execution is isolated via agent-env
	// (the agentm pod model) rather than run as a host subprocess
	// (claude-code / codex).
	Sandboxed bool
	// ManagesOwnLabels reports whether the agent self-edits issue labels
	// from inside its own subprocess (claude-code / codex via
	// `gh issue edit`). When false, the runtime cannot touch labels itself
	// and the coordinator-side LabelWriter owns the state-machine transition
	// (agentm). This flag drives the label-write wiring in
	// internal/launcher.
	ManagesOwnLabels bool
	// NeedsHostGHCreds reports whether the runtime needs the host gh/git
	// credentials available in-process (claude-code / codex). Sandboxed
	// runtimes carry their own credentials inside agent-env (agentm).
	NeedsHostGHCreds bool
}
