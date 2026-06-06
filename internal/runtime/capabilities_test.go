package runtime

import (
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// TestCapabilitiesPerRuntime pins the capability descriptor each built-in
// runtime publishes (ADR 2026-06-06 §1). These values are load-bearing: the
// label-write wiring is driven off ManagesOwnLabels, and a future claim
// filter / benchmark harness reads Sandboxed + NeedsHostGHCreds.
func TestCapabilitiesPerRuntime(t *testing.T) {
	selfManaged := Capabilities{Sandboxed: false, ManagesOwnLabels: true, NeedsHostGHCreds: true}
	sandboxed := Capabilities{Sandboxed: true, ManagesOwnLabels: false, NeedsHostGHCreds: false}

	cases := []struct {
		name string
		rt   Runtime
		want Capabilities
	}{
		{"claude", &ClaudeRuntime{}, selfManaged},
		{"codex-bridge", NewAgentBridgeRuntime(config.RuntimeCodex, nil), selfManaged},
		{"agentm", NewAgentMRuntime(nil), sandboxed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rt.Capabilities(); got != tc.want {
				t.Fatalf("%s Capabilities() = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

// TestAgentMSetLabelWriterGatedByCapability asserts the capability-driven
// wiring: a Registry.SetLabelWriter installs the writer on the agentm runtime
// (ManagesOwnLabels=false) and leaves the self-managed bridge runtime
// (ManagesOwnLabels=true) untouched.
func TestAgentMSetLabelWriterGatedByCapability(t *testing.T) {
	reg := NewRegistry()
	codex := NewAgentBridgeRuntime(config.RuntimeCodex, nil)
	agentM := NewAgentMRuntime(nil)
	reg.Register(codex, config.RuntimeCodex)
	reg.Register(agentM, config.RuntimeAgentM)

	lw := &fakeLabelWriter{}
	reg.SetLabelWriter(lw)

	if agentM.LabelWriter != lw {
		t.Fatalf("agentm runtime (ManagesOwnLabels=false) must receive the label writer")
	}
	// codex has no LabelWriter field at all; the capability gate (and the
	// missing SetLabelWriter method) is what keeps it out. Re-assert the
	// gate explicitly so a future regression on Capabilities() is caught.
	if !codex.Capabilities().ManagesOwnLabels {
		t.Fatalf("codex bridge runtime must report ManagesOwnLabels=true so wiring skips it")
	}
}
