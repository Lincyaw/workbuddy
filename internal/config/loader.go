// Package config loads and validates workbuddy configuration files.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Warning represents a non-fatal configuration issue.
type Warning struct {
	Message string
}

func (w Warning) String() string {
	return w.Message
}

// validRuntimes enumerates allowed agent runtime values.
var validRuntimes = map[string]bool{
	"claude-code": true,
	"codex":       true,
}

// LoadConfig loads the full configuration from the given config directory.
// It returns the parsed config, a list of non-fatal warnings, and any error.
func LoadConfig(configDir string) (*FullConfig, []Warning, error) {
	info, err := os.Stat(configDir)
	if err != nil {
		return nil, nil, fmt.Errorf("config: directory %q: %w", configDir, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("config: %q is not a directory", configDir)
	}

	cfg := &FullConfig{
		Agents:    make(map[string]*AgentConfig),
		Workflows: make(map[string]*WorkflowConfig),
	}
	var warnings []Warning

	// Load global config.yaml (optional — if present).
	globalPath := filepath.Join(configDir, "config.yaml")
	if data, err := os.ReadFile(globalPath); err == nil {
		if err := yaml.Unmarshal(data, &cfg.Global); err != nil {
			return nil, nil, fmt.Errorf("config: %s: %w", globalPath, err)
		}
	}

	// Load agents.
	agentsDir := filepath.Join(configDir, "agents")
	if entries, err := os.ReadDir(agentsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(agentsDir, e.Name())
			agent, err := parseAgentFile(path)
			if err != nil {
				return nil, nil, err
			}
			cfg.Agents[agent.Name] = agent
		}
	}

	// Load workflows.
	workflowsDir := filepath.Join(configDir, "workflows")
	if entries, err := os.ReadDir(workflowsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(workflowsDir, e.Name())
			wf, err := parseWorkflowFile(path)
			if err != nil {
				return nil, nil, err
			}
			cfg.Workflows[wf.Name] = wf
		}
	}

	// Cross-config validations.
	if err := validateWorkflowTriggerConflicts(cfg.Workflows); err != nil {
		return nil, nil, err
	}

	warnings = append(warnings, checkAgentLabelConsistency(cfg)...)

	return cfg, warnings, nil
}

// parseFrontmatter splits a Markdown file into YAML frontmatter and body.
func parseFrontmatter(data []byte) (frontmatter []byte, body string, err error) {
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return nil, "", fmt.Errorf("missing YAML frontmatter delimiter")
	}
	rest := content[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, "", fmt.Errorf("missing closing YAML frontmatter delimiter")
	}
	fm := rest[:idx]
	body = rest[idx+4:] // skip "\n---"
	return []byte(fm), body, nil
}

// yamlCodeBlockRe matches the first ```yaml ... ``` code block.
var yamlCodeBlockRe = regexp.MustCompile("(?s)```yaml\\s*\n(.*?)```")

// parseAgentFile parses an agent Markdown file with YAML frontmatter.
func parseAgentFile(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	fm, _, err := parseFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	var agent AgentConfig
	if err := yaml.Unmarshal(fm, &agent); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	// Validate required fields.
	fname := filepath.Base(path)
	if agent.Name == "" {
		return nil, fmt.Errorf("config: %s: missing required field \"name\"", fname)
	}
	if len(agent.Triggers) == 0 {
		return nil, fmt.Errorf("config: %s: missing required field \"triggers\"", fname)
	}
	if agent.Role == "" {
		return nil, fmt.Errorf("config: %s: missing required field \"role\"", fname)
	}
	if agent.Command == "" {
		return nil, fmt.Errorf("config: %s: missing required field \"command\"", fname)
	}

	// Default runtime.
	if agent.Runtime == "" {
		agent.Runtime = "claude-code"
	}
	if !validRuntimes[agent.Runtime] {
		return nil, fmt.Errorf("config: %s: invalid runtime %q (valid: claude-code, codex)", fname, agent.Runtime)
	}

	return &agent, nil
}

// parseWorkflowFile parses a workflow Markdown file with YAML frontmatter
// and an embedded YAML code block for states.
func parseWorkflowFile(path string) (*WorkflowConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	fm, body, err := parseFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	var wf WorkflowConfig
	if err := yaml.Unmarshal(fm, &wf); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	// Default max_retries.
	if wf.MaxRetries == 0 {
		wf.MaxRetries = 3
	}

	// Extract states from the first ```yaml code block.
	fname := filepath.Base(path)
	match := yamlCodeBlockRe.FindStringSubmatch(body)
	if match == nil {
		return nil, fmt.Errorf("config: %s: no yaml code block found for states definition", fname)
	}

	var block StatesBlock
	if err := yaml.Unmarshal([]byte(match[1]), &block); err != nil {
		return nil, fmt.Errorf("config: %s: states yaml: %w", fname, err)
	}
	wf.States = block.States

	// Validate states and edges.
	if err := validateStates(fname, &wf); err != nil {
		return nil, err
	}

	// Auto-add failed state if workflow has back-edges but no failed state.
	autoAddFailedState(&wf)

	return &wf, nil
}

// validateStates checks the single-edge constraint on workflow states.
func validateStates(fname string, wf *WorkflowConfig) error {
	for stateName, state := range wf.States {
		seen := make(map[string]bool)
		for _, t := range state.Transitions {
			key := stateName + "->" + t.To
			if seen[key] {
				return fmt.Errorf("config: %s: duplicate transition edge from %q to %q", fname, stateName, t.To)
			}
			seen[key] = true
		}
	}
	return nil
}

// autoAddFailedState adds a "failed" state if the workflow has back-edges
// (a transition where To references a state that appears earlier or the same
// state, indicating a cycle/retry) but no explicit "failed" state.
func autoAddFailedState(wf *WorkflowConfig) {
	if _, exists := wf.States[StateNameFailed]; exists {
		return
	}
	// Check for back-edges: any transition whose To target is a state that
	// also has outgoing transitions (i.e., not a terminal state), creating a cycle.
	hasBackEdge := false
	for _, state := range wf.States {
		for _, t := range state.Transitions {
			if target, ok := wf.States[t.To]; ok {
				if len(target.Transitions) > 0 {
					hasBackEdge = true
					break
				}
			}
		}
		if hasBackEdge {
			break
		}
	}

	if hasBackEdge {
		wf.States[StateNameFailed] = &State{
			EnterLabel: LabelFailed,
		}
	}
}

// validateWorkflowTriggerConflicts checks that no two workflows share the
// same trigger.issue_label.
func validateWorkflowTriggerConflicts(workflows map[string]*WorkflowConfig) error {
	seen := make(map[string]string) // label -> workflow name
	for _, wf := range workflows {
		label := wf.Trigger.IssueLabel
		if label == "" {
			continue
		}
		if prev, ok := seen[label]; ok {
			return fmt.Errorf("config: workflow trigger conflict: workflows %q and %q both trigger on label %q", prev, wf.Name, label)
		}
		seen[label] = wf.Name
	}
	return nil
}

// checkAgentLabelConsistency returns warnings for agent trigger labels that
// don't appear as enter_label in any workflow state.
func checkAgentLabelConsistency(cfg *FullConfig) []Warning {
	// Collect all enter_labels from workflows.
	enterLabels := make(map[string]bool)
	for _, wf := range cfg.Workflows {
		for _, state := range wf.States {
			if state.EnterLabel != "" {
				enterLabels[state.EnterLabel] = true
			}
		}
	}

	var warnings []Warning
	for _, agent := range cfg.Agents {
		for _, trigger := range agent.Triggers {
			if trigger.Label != "" && !enterLabels[trigger.Label] {
				warnings = append(warnings, Warning{
					Message: fmt.Sprintf("agent %q trigger label %q does not match any workflow state enter_label", agent.Name, trigger.Label),
				})
			}
		}
	}
	return warnings
}
