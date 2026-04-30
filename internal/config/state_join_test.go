package config

import (
	"encoding/json"
	"testing"
)

// TestJoinConfigJSONLegacyScalar pins down the round-trip path used by
// persisted SQLite coordinator registrations. Old registrations written
// before rollouts phase 1 stored `join` as a bare string scalar; the new
// JoinConfig struct must keep decoding them or the coordinator crashes on
// boot with `cannot unmarshal string into Go struct field`.
func TestJoinConfigJSONLegacyScalar(t *testing.T) {
	var j JoinConfig
	if err := json.Unmarshal([]byte(`"all_passed"`), &j); err != nil {
		t.Fatalf("legacy scalar must decode: %v", err)
	}
	if j.Strategy != "all_passed" {
		t.Fatalf("strategy: got %q want %q", j.Strategy, "all_passed")
	}
	if j.MinSuccesses != 0 {
		t.Fatalf("min_successes: got %d want 0", j.MinSuccesses)
	}
}

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

// TestStateJSONLegacyScalarJoin exercises the inner field path: registrations
// embed JoinConfig inside State, so the failing prod path is decoding a State
// JSON blob whose `join` is a bare string.
func TestStateJSONLegacyScalarJoin(t *testing.T) {
	raw := []byte(`{"EnterLabel":"developing","Join":"all_passed","Transitions":{"status:reviewing":"reviewing"}}`)
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("State with legacy scalar join must decode: %v", err)
	}
	if s.Join.Strategy != "all_passed" {
		t.Fatalf("join.strategy = %q", s.Join.Strategy)
	}
}
