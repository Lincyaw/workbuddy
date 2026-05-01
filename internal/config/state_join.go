package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// UnmarshalJSON accepts both legacy scalar joins persisted as bare strings
// (`"join": "all_passed"`) and the rollout-aware mapping form
// (`"join": {"strategy": "rollouts", "min_successes": 2}`). This mirrors
// State.UnmarshalYAML so configs that round-trip through SQLite registration
// JSON keep decoding after the rollouts phase-1 schema change. Without this
// method, persisted registrations written before the JoinConfig struct
// existed (where `Join` was a string scalar) will fail with
// `cannot unmarshal string into Go struct field`.
func (j *JoinConfig) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*j = JoinConfig{}
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("config: decode legacy join scalar: %w", err)
		}
		*j = JoinConfig{Strategy: s}
		return nil
	}
	type joinAlias JoinConfig
	var aux joinAlias
	if err := json.Unmarshal(trimmed, &aux); err != nil {
		return fmt.Errorf("config: decode join object: %w", err)
	}
	*j = JoinConfig(aux)
	return nil
}

// UnmarshalYAML accepts both legacy scalar joins (`join: all_passed`) and the
// rollout-aware mapping form (`join: {strategy: rollouts, min_successes: 2}`).
func (s *State) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("config: state must be a mapping")
	}
	if err := validateMappingKeys(value, map[string]struct{}{
		"enter_label": {},
		"agent":       {},
		"agents":      {},
		"mode":        {},
		"join":        {},
		"rollouts":    {},
		"transitions": {},
	}); err != nil {
		return err
	}

	type rawState struct {
		EnterLabel  string            `yaml:"enter_label"`
		Agent       string            `yaml:"agent,omitempty"`
		Agents      []string          `yaml:"agents,omitempty"`
		Mode        string            `yaml:"mode,omitempty"`
		Rollouts    int               `yaml:"rollouts,omitempty"`
		Transitions map[string]string `yaml:"transitions"`
	}
	var raw struct {
		rawState `yaml:",inline"`
		Join     yaml.Node `yaml:"join"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	s.EnterLabel = raw.EnterLabel
	s.Agent = raw.Agent
	s.Agents = raw.Agents
	s.Mode = raw.Mode
	s.Rollouts = raw.Rollouts
	s.Transitions = raw.Transitions
	switch s.Mode {
	case "", StateModeReview, StateModeSynth:
	default:
		return fmt.Errorf("config: invalid mode %q (valid: %q, %q)", s.Mode, StateModeReview, StateModeSynth)
	}
	if raw.Join.Kind == 0 {
		s.Join = JoinConfig{}
		return nil
	}
	switch raw.Join.Kind {
	case yaml.ScalarNode:
		if err := raw.Join.Decode(&s.Join.Strategy); err != nil {
			return fmt.Errorf("config: decode join strategy: %w", err)
		}
	case yaml.MappingNode:
		if err := validateMappingKeys(&raw.Join, map[string]struct{}{
			"strategy":      {},
			"min_successes": {},
		}); err != nil {
			return err
		}
		if err := raw.Join.Decode(&s.Join); err != nil {
			return fmt.Errorf("config: decode join config: %w", err)
		}
	default:
		return fmt.Errorf("config: join must be a string or mapping")
	}
	return nil
}

func validateMappingKeys(node *yaml.Node, allowed map[string]struct{}) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if _, ok := allowed[key.Value]; ok {
			continue
		}
		return fmt.Errorf("config: unknown key %q", key.Value)
	}
	return nil
}
