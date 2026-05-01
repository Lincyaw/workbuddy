package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestJoinConfigJSONObject(t *testing.T) {
	// Persisted registrations use Go default (CamelCase) field naming because
	// JoinConfig has no explicit json tags. Round-trip must keep that form
	// decodable.
	var j JoinConfig
	if err := json.Unmarshal([]byte(`{"Strategy":"rollouts","MinSuccesses":2}`), &j); err != nil {
		t.Fatalf("object form must decode: %v", err)
	}
	if j.Strategy != "rollouts" || j.MinSuccesses != 2 {
		t.Fatalf("decoded join = %+v", j)
	}
}

func TestJoinConfigJSONNullAndEmpty(t *testing.T) {
	var j JoinConfig
	if err := json.Unmarshal([]byte(`null`), &j); err != nil {
		t.Fatalf("null must decode: %v", err)
	}
	if j != (JoinConfig{}) {
		t.Fatalf("null should produce zero value, got %+v", j)
	}
}

func TestStateYAMLModeValidation(t *testing.T) {
	var state State
	if err := yaml.Unmarshal([]byte(`
enter_label: status:synthesizing
agent: review-agent
mode: synthesize
transitions:
  status:reviewing: reviewing
`), &state); err != nil {
		t.Fatalf("valid synth mode must decode: %v", err)
	}
	if state.Mode != StateModeSynth {
		t.Fatalf("mode = %q, want %q", state.Mode, StateModeSynth)
	}

	if err := yaml.Unmarshal([]byte(`
enter_label: status:synthesizing
mode: invalid
transitions: {}
`), &state); err == nil {
		t.Fatal("expected invalid mode to be rejected")
	}
}
