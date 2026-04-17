// Package config loads and validates workbuddy configuration files.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Warning represents a non-fatal configuration issue.
type Warning struct {
	Message string
}

func (w Warning) String() string {
	return w.Message
}

const (
	RunnerLocal         = "local"
	RunnerGitHubActions = "github-actions"

	RuntimeClaudeCode  = "claude-code"
	RuntimeClaudeShot  = "claude-oneshot"
	RuntimeCodex       = "codex"
	RuntimeCodexExec   = "codex-exec"
	RuntimeCodexServer = "codex-appserver"
)

const (
	JoinAllPassed = "all_passed"
	JoinAnyPassed = "any_passed"
)

var validRuntimes = map[string]bool{
	RuntimeClaudeCode:  true,
	RuntimeClaudeShot:  true,
	RuntimeCodex:       true,
	RuntimeCodexExec:   true,
	RuntimeCodexServer: true,
}

var publicRuntimes = []string{RuntimeClaudeCode, RuntimeCodex, RuntimeCodexServer}
var validRunners = map[string]bool{
	RunnerLocal:         true,
	RunnerGitHubActions: true,
}

const (
	defaultStaleInferenceIdleThreshold = 5 * time.Minute
	defaultStaleInferenceCheckInterval = 30 * time.Second
)

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

	globalPath := filepath.Join(configDir, "config.yaml")
	if data, err := os.ReadFile(globalPath); err == nil {
		var fileCfg struct {
			GlobalConfig  `yaml:",inline"`
			Notifications NotificationsConfig `yaml:"notifications"`
		}
		if err := yaml.Unmarshal(data, &fileCfg); err != nil {
			return nil, nil, fmt.Errorf("config: %s: %w", globalPath, err)
		}
		cfg.Global = fileCfg.GlobalConfig
		cfg.Notifications = fileCfg.Notifications
	}

	agentsDir := filepath.Join(configDir, "agents")
	if entries, err := os.ReadDir(agentsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(agentsDir, e.Name())
			agent, agentWarnings, err := parseAgentFile(path)
			if err != nil {
				return nil, nil, err
			}
			cfg.Agents[agent.Name] = agent
			warnings = append(warnings, agentWarnings...)
		}
	}

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

	if err := validateWorkflowTriggerConflicts(cfg.Workflows); err != nil {
		return nil, nil, err
	}

	warnings = append(warnings, checkAgentLabelConsistency(cfg)...)

	return cfg, warnings, nil
}

// ValidateWorkflowRegistration validates a workflow map for registration payloads.
func ValidateWorkflowRegistration(workflows map[string]*WorkflowConfig) error {
	return validateWorkflowTriggerConflicts(workflows)
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
	body = rest[idx+4:]
	return []byte(fm), body, nil
}

var yamlCodeBlockRe = regexp.MustCompile("(?s)```yaml\\s*\n(.*?)```")

func parseAgentFile(path string) (*AgentConfig, []Warning, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %s: %w", path, err)
	}
	fm, _, err := parseFrontmatter(data)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %s: %w", path, err)
	}

	var agent AgentConfig
	if err := yaml.Unmarshal(fm, &agent); err != nil {
		return nil, nil, fmt.Errorf("config: %s: %w", path, err)
	}
	agent.SourcePath = path

	fname := filepath.Base(path)
	if agent.Name == "" {
		return nil, nil, fmt.Errorf("config: %s: missing required field \"name\"", fname)
	}
	if len(agent.Triggers) == 0 {
		return nil, nil, fmt.Errorf("config: %s: missing required field \"triggers\"", fname)
	}
	if agent.Role == "" {
		return nil, nil, fmt.Errorf("config: %s: missing required field \"role\"", fname)
	}
	if agent.Command == "" && strings.TrimSpace(agent.Prompt) == "" {
		return nil, nil, fmt.Errorf("config: %s: missing required field \"command\" or \"prompt\"", fname)
	}

	if agent.Runtime == "" {
		agent.Runtime = RuntimeClaudeCode
	}
	if agent.Runner == "" {
		agent.Runner = RunnerLocal
	}
	if !validRunners[agent.Runner] {
		return nil, nil, fmt.Errorf("config: %s: invalid runner %q (valid: %s, %s)", fname, agent.Runner, RunnerLocal, RunnerGitHubActions)
	}
	if !validRuntimes[agent.Runtime] {
		return nil, nil, fmt.Errorf("config: %s: invalid runtime %q (valid: %s)", fname, agent.Runtime, strings.Join(publicRuntimes, ", "))
	}

	warnings, err := normalizeAgentConfig(&agent)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %s: %w", fname, err)
	}
	return &agent, warnings, nil
}

// NormalizeAgentConfig applies runtime aliases/defaults and validates the
// runtime/policy matrix for an ad hoc agent config.
func NormalizeAgentConfig(agent *AgentConfig) ([]Warning, error) {
	return normalizeAgentConfig(agent)
}

func normalizeAgentConfig(agent *AgentConfig) ([]Warning, error) {
	var warnings []Warning

	switch agent.Runtime {
	case RuntimeCodex:
		agent.Runtime = RuntimeCodexExec
	}

	if agent.Policy.Timeout > 0 {
		agent.Timeout = agent.Policy.Timeout
	}
	if agent.Runner == "" {
		agent.Runner = RunnerLocal
	}
	switch agent.Runner {
	case RunnerLocal:
	case RunnerGitHubActions:
		if agent.GitHubActions.Workflow == "" {
			agent.GitHubActions.Workflow = "workbuddy-remote-runner.yml"
		}
		if agent.GitHubActions.PollInterval <= 0 {
			agent.GitHubActions.PollInterval = 5 * time.Second
		}
	default:
		return warnings, fmt.Errorf("unsupported runner %q", agent.Runner)
	}
	if agent.OutputContract.SchemaFile != "" {
		if strings.TrimSpace(agent.OutputContract.SchemaFile) == "" {
			return warnings, fmt.Errorf("output_contract.schema_file cannot be blank")
		}
		schemaPath := agent.OutputContractSchemaPath()
		if schemaPath == "" {
			return warnings, fmt.Errorf("output_contract.schema_file requires agent.SourcePath")
		}
		if _, err := os.Stat(schemaPath); err != nil {
			return warnings, fmt.Errorf("output_contract.schema_file %q: %w", agent.OutputContract.SchemaFile, err)
		}
	}

	if agent.Policy.Sandbox == "" {
		agent.Policy.Sandbox = defaultSandboxForRuntime(agent.Runtime)
	}
	if agent.Policy.Approval == "" {
		agent.Policy.Approval = defaultApprovalForRuntime(agent.Runtime)
	}

	if agent.Policy.Sandbox == "" {
		return warnings, fmt.Errorf("policy.sandbox is required for runtime %q", agent.Runtime)
	}
	if agent.Policy.Approval == "" {
		return warnings, fmt.Errorf("policy.approval is required for runtime %q", agent.Runtime)
	}

	switch agent.Runtime {
	case RuntimeClaudeCode, RuntimeClaudeShot:
		switch agent.Policy.Sandbox {
		case "read-only", "danger-full-access":
		case "workspace-write":
			warnings = append(warnings, Warning{Message: fmt.Sprintf("agent %q: runtime %q does not support workspace-write sandbox; degrading to read-only semantics", agent.Name, agent.Runtime)})
			agent.Policy.Sandbox = "read-only"
		default:
			return warnings, fmt.Errorf("unsupported policy.sandbox %q for runtime %q", agent.Policy.Sandbox, agent.Runtime)
		}
		if agent.Policy.Approval != "never" {
			return warnings, fmt.Errorf("unsupported policy.approval %q for runtime %q", agent.Policy.Approval, agent.Runtime)
		}
	case RuntimeCodexExec:
		switch agent.Policy.Sandbox {
		case "read-only", "workspace-write", "danger-full-access":
		default:
			return warnings, fmt.Errorf("unsupported policy.sandbox %q for runtime %q", agent.Policy.Sandbox, agent.Runtime)
		}
		switch agent.Policy.Approval {
		case "never", "on-failure", "on-request":
		default:
			return warnings, fmt.Errorf("unsupported policy.approval %q for runtime %q", agent.Policy.Approval, agent.Runtime)
		}
	case RuntimeCodexServer:
		switch agent.Policy.Sandbox {
		case "read-only", "workspace-write", "danger-full-access":
		default:
			return warnings, fmt.Errorf("unsupported policy.sandbox %q for runtime %q", agent.Policy.Sandbox, agent.Runtime)
		}
		switch agent.Policy.Approval {
		case "never", "on-failure", "on-request", "via-approver":
		default:
			return warnings, fmt.Errorf("unsupported policy.approval %q for runtime %q", agent.Policy.Approval, agent.Runtime)
		}
	default:
		return warnings, fmt.Errorf("unsupported runtime %q", agent.Runtime)
	}

	return warnings, nil
}

// OutputContractSchemaPath resolves the output_contract.schema_file against the
// agent file path so agent-local schema folders work without global path rules.
func (a *AgentConfig) OutputContractSchemaPath() string {
	schemaFile := strings.TrimSpace(a.OutputContract.SchemaFile)
	if schemaFile == "" {
		return ""
	}
	if filepath.IsAbs(schemaFile) {
		return schemaFile
	}
	if strings.TrimSpace(a.SourcePath) == "" {
		return ""
	}
	return filepath.Clean(filepath.Join(filepath.Dir(a.SourcePath), schemaFile))
}

func defaultSandboxForRuntime(runtime string) string {
	switch runtime {
	case RuntimeClaudeCode, RuntimeClaudeShot:
		return "read-only"
	case RuntimeCodexExec:
		return "read-only"
	case RuntimeCodexServer:
		return "read-only"
	default:
		return ""
	}
}

func defaultApprovalForRuntime(runtime string) string {
	switch runtime {
	case RuntimeClaudeCode, RuntimeClaudeShot, RuntimeCodexExec, RuntimeCodexServer:
		return "never"
	default:
		return ""
	}
}

func (cfg *FullConfig) EffectiveStaleInference(agent *AgentConfig) EffectiveStaleInferenceConfig {
	effective := EffectiveStaleInferenceConfig{
		Enabled:       true,
		IdleThreshold: defaultStaleInferenceIdleThreshold,
		CheckInterval: defaultStaleInferenceCheckInterval,
	}
	if cfg != nil {
		mergeStaleInferenceConfig(&effective, cfg.Global.Worker.StaleInference)
	}
	if agent != nil {
		mergeStaleInferenceConfig(&effective, agent.Policy.StaleInference)
	}
	return effective
}

func mergeStaleInferenceConfig(dst *EffectiveStaleInferenceConfig, src StaleInferenceConfig) {
	if dst == nil {
		return
	}
	if src.Enabled != nil {
		dst.Enabled = *src.Enabled
	}
	if src.IdleThreshold > 0 {
		dst.IdleThreshold = src.IdleThreshold
	}
	if src.CheckInterval > 0 {
		dst.CheckInterval = src.CheckInterval
	}
}

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

	if wf.MaxRetries == 0 {
		wf.MaxRetries = 3
	}

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

	if err := normalizeWorkflowStates(fname, &wf); err != nil {
		return nil, err
	}
	if err := validateStates(fname, &wf); err != nil {
		return nil, err
	}

	autoAddFailedState(&wf)
	return &wf, nil
}

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

func normalizeWorkflowStates(fname string, wf *WorkflowConfig) error {
	for stateName, state := range wf.States {
		if state == nil {
			return fmt.Errorf("config: %s: state %q cannot be nil", fname, stateName)
		}

		if len(state.Agents) == 0 && strings.TrimSpace(state.Agent) != "" {
			state.Agents = []string{state.Agent}
		}

		if len(state.Agents) == 0 {
			state.Join = ""
			continue
		}

		join := strings.TrimSpace(state.Join)
		if join == "" {
			join = JoinAllPassed
		}
		switch join {
		case JoinAllPassed, JoinAnyPassed:
		default:
			return fmt.Errorf("config: %s: invalid join %q for state %q (expected %q or %q)", fname, join, stateName, JoinAllPassed, JoinAnyPassed)
		}
		state.Join = join

		seenAgents := make(map[string]struct{}, len(state.Agents))
		normalizedAgents := make([]string, 0, len(state.Agents))
		for _, agent := range state.Agents {
			agent = strings.TrimSpace(agent)
			if agent == "" {
				return fmt.Errorf("config: %s: empty agent name in state %q agents list", fname, stateName)
			}
			if _, exists := seenAgents[agent]; exists {
				return fmt.Errorf("config: %s: duplicate agent %q in state %q", fname, agent, stateName)
			}
			seenAgents[agent] = struct{}{}
			normalizedAgents = append(normalizedAgents, agent)
		}
		state.Agents = normalizedAgents
	}
	return nil
}

func autoAddFailedState(wf *WorkflowConfig) {
	if _, exists := wf.States[StateNameFailed]; exists {
		return
	}
	for _, state := range wf.States {
		for _, t := range state.Transitions {
			if target, ok := wf.States[t.To]; ok && len(target.Transitions) > 0 {
				wf.States[StateNameFailed] = &State{EnterLabel: LabelFailed}
				return
			}
		}
	}
}

func validateWorkflowTriggerConflicts(workflows map[string]*WorkflowConfig) error {
	seen := make(map[string]string)
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

func checkAgentLabelConsistency(cfg *FullConfig) []Warning {
	enterLabels := make(map[string]bool)
	for _, wf := range cfg.Workflows {
		for _, state := range wf.States {
			if state.EnterLabel != "" {
				enterLabels[state.EnterLabel] = true
			}
		}
	}

	var warnings []Warning
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		agent := cfg.Agents[name]
		for _, trigger := range agent.Triggers {
			if trigger.Label != "" && !enterLabels[trigger.Label] {
				warnings = append(warnings, Warning{Message: fmt.Sprintf("agent %q trigger label %q does not match any workflow state enter_label", agent.Name, trigger.Label)})
			}
		}
	}
	return warnings
}
