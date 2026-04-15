package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// helper to create a temp config directory with files.
func setupConfigDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for relPath, content := range files {
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const validAgent = `---
name: dev-agent
description: Dev agent
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: claude-code
command: "claude -p 'do stuff'"
timeout: 30m
---
## Dev Agent
`

const validCodexAgent = `---
name: review-agent
description: Review agent (codex runtime)
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: codex
policy:
  sandbox: danger-full-access
  approval: never
  model: gpt-5.4
  timeout: 25m
prompt: |
  verify issue {{.Issue.Number}}
command: "codex exec legacy"
---
## Review Agent
`

const validWorkflow = `---
name: feature-dev
description: Feature development
trigger:
  issue_label: "type:feature"
max_retries: 5
---
## Feature Dev

` + "```yaml" + `
states:
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: reviewing
        when: "labeled:status:reviewing"
      - to: blocked
        when: "labeled:status:blocked"
  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      - to: done
        when: "labeled:status:done"
      - to: developing
        when: "labeled:status:developing"
  blocked:
    enter_label: "status:blocked"
    transitions:
      - to: developing
        when: "labeled:status:developing"
  done:
    enter_label: "status:done"
` + "```" + `
`

const validGlobalConfig = `repo: owner/repo
environment: production
poll_interval: 30s
port: 8080
`

func TestRepositorySampleConfig_MatchesGlobalConfigSchema(t *testing.T) {
	samplePath := filepath.Join("..", "..", ".github", "workbuddy", "config.yaml")
	data, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatalf("read sample config: %v", err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal sample config: %v", err)
	}

	expectedKeys := map[string]struct{}{
		"environment":   {},
		"poll_interval": {},
		"port":          {},
		"repo":          {},
	}

	if len(raw) != len(expectedKeys) {
		t.Fatalf("top-level key count = %d, want %d (%v)", len(raw), len(expectedKeys), raw)
	}
	for key := range expectedKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing top-level key %q", key)
		}
	}
	for key := range raw {
		if _, ok := expectedKeys[key]; !ok {
			t.Fatalf("unexpected top-level key %q in repository sample config", key)
		}
	}

	var cfg GlobalConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal sample config into GlobalConfig: %v", err)
	}
	if cfg.Repo != "Lincyaw/workbuddy" {
		t.Fatalf("repo = %q, want %q", cfg.Repo, "Lincyaw/workbuddy")
	}
	if cfg.Environment != "dev" {
		t.Fatalf("environment = %q, want %q", cfg.Environment, "dev")
	}
	if cfg.PollInterval != 30*time.Second {
		t.Fatalf("poll_interval = %s, want 30s", cfg.PollInterval)
	}
	if cfg.Port != 8080 {
		t.Fatalf("port = %d, want 8080", cfg.Port)
	}
}

func TestRepositorySampleConfig_LoadsMinimalAgentCatalog(t *testing.T) {
	configDir := filepath.Join("..", "..", ".github", "workbuddy")

	cfg, warnings, err := LoadConfig(configDir)
	if err != nil {
		t.Fatalf("LoadConfig(repository sample): %v", err)
	}

	expectedAgents := []string{
		"dev-agent",
		"review-agent",
	}
	for _, name := range expectedAgents {
		if _, ok := cfg.Agents[name]; !ok {
			t.Fatalf("repository sample config missing agent %q", name)
		}
	}
	if got := len(cfg.Agents); got != len(expectedAgents) {
		t.Fatalf("repository sample config agent count = %d, want %d; catalog is now minimal (dev + review only)", got, len(expectedAgents))
	}

	if got := cfg.Agents["dev-agent"].Runtime; got != RuntimeCodexExec {
		t.Fatalf("dev-agent runtime = %q, want %q", got, RuntimeCodexExec)
	}
	if got := cfg.Agents["review-agent"].Runtime; got != RuntimeCodexExec {
		t.Fatalf("review-agent runtime = %q, want %q", got, RuntimeCodexExec)
	}

	// Both agents' trigger labels (status:developing, status:reviewing) should be
	// covered by the workflow state machine, so no trigger-label warnings expected.
	for _, w := range warnings {
		if strings.Contains(w.Message, "status:") {
			t.Fatalf("unexpected trigger-label warning in minimal 2-agent catalog: %q", w.Message)
		}
	}
}

// Enforces the 2-agent catalog at the filesystem level so the len() check in
// TestRepositorySampleConfig_LoadsMinimalAgentCatalog cannot be silently
// defeated by someone adding a 3rd agent file and bumping the expected list.
func TestRepositoryAgentsDirectoryIsMinimal(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("..", "..", ".github", "workbuddy", "agents", "*.md"))
	if err != nil {
		t.Fatalf("glob agents dir: %v", err)
	}
	got := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		got[filepath.Base(e)] = struct{}{}
	}
	want := map[string]struct{}{
		"dev-agent.md":    {},
		"review-agent.md": {},
	}
	if len(got) != len(want) {
		t.Fatalf("agents dir has %d .md files, want %d (%v)", len(got), len(want), entries)
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("agents dir missing %s", name)
		}
	}
}

// Test 1: Normal parse — agents, workflows, and global config all load correctly.
func TestLoadConfig_NormalParse(t *testing.T) {
	dir := setupConfigDir(t, map[string]string{
		"config.yaml":              validGlobalConfig,
		"agents/dev-agent.md":      validAgent,
		"agents/review-agent.md":   validCodexAgent,
		"workflows/feature-dev.md": validWorkflow,
	})

	cfg, warnings, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check global config.
	if cfg.Global.Repo != "owner/repo" {
		t.Errorf("repo = %q, want %q", cfg.Global.Repo, "owner/repo")
	}
	if cfg.Global.Port != 8080 {
		t.Errorf("port = %d, want 8080", cfg.Global.Port)
	}

	// Check agent.
	agent, ok := cfg.Agents["dev-agent"]
	if !ok {
		t.Fatal("agent 'dev-agent' not found")
	}
	if agent.Role != "dev" {
		t.Errorf("agent role = %q, want %q", agent.Role, "dev")
	}
	if agent.Runtime != "claude-code" {
		t.Errorf("agent runtime = %q, want %q", agent.Runtime, "claude-code")
	}

	codexAgent, ok := cfg.Agents["review-agent"]
	if !ok {
		t.Fatal("agent 'review-agent' not found")
	}
	if codexAgent.Runtime != RuntimeCodexExec {
		t.Errorf("codex runtime = %q, want %q", codexAgent.Runtime, RuntimeCodexExec)
	}
	if codexAgent.Policy.Model != "gpt-5.4" {
		t.Errorf("codex model = %q", codexAgent.Policy.Model)
	}
	if codexAgent.Timeout != 25*time.Minute {
		t.Errorf("codex timeout = %s", codexAgent.Timeout)
	}
	if strings.TrimSpace(codexAgent.Prompt) == "" {
		t.Fatal("expected prompt to be parsed")
	}

	// Check workflow.
	wf, ok := cfg.Workflows["feature-dev"]
	if !ok {
		t.Fatal("workflow 'feature-dev' not found")
	}
	if wf.MaxRetries != 5 {
		t.Errorf("max_retries = %d, want 5", wf.MaxRetries)
	}
	if len(wf.States) < 4 {
		t.Errorf("states count = %d, want >= 4", len(wf.States))
	}

	// Agent label matches workflow enter_label, so no warning expected.
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestNormalizeAgentConfig_RejectsUnsupportedCodexExecApproval(t *testing.T) {
	agent := &AgentConfig{
		Name:    "codex-agent",
		Runtime: RuntimeCodex,
		Prompt:  "do work",
		Policy: PolicyConfig{
			Sandbox:  "danger-full-access",
			Approval: "via-approver",
		},
	}

	_, err := NormalizeAgentConfig(agent)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `unsupported policy.approval "via-approver"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeAgentConfig_ClaudeWorkspaceWriteWarns(t *testing.T) {
	agent := &AgentConfig{
		Name:    "claude-agent",
		Runtime: RuntimeClaudeCode,
		Command: "echo hello",
		Policy: PolicyConfig{
			Sandbox:  "workspace-write",
			Approval: "never",
		},
	}

	warnings, err := NormalizeAgentConfig(agent)
	if err != nil {
		t.Fatalf("NormalizeAgentConfig: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0].Message, "workspace-write") {
		t.Fatalf("warning = %q", warnings[0].Message)
	}
	if agent.Policy.Sandbox != "read-only" {
		t.Fatalf("expected sandbox to be normalized to read-only, got %q", agent.Policy.Sandbox)
	}
}

func TestNormalizeAgentConfig_CodexAppServerAllowsViaApprover(t *testing.T) {
	agent := &AgentConfig{
		Name:    "appserver-agent",
		Runtime: RuntimeCodexServer,
		Prompt:  "hello",
		Policy: PolicyConfig{
			Sandbox:  "workspace-write",
			Approval: "via-approver",
		},
	}

	if _, err := NormalizeAgentConfig(agent); err != nil {
		t.Fatalf("NormalizeAgentConfig: %v", err)
	}
}

// Test 2: Missing required field — agent without 'name'.
func TestLoadConfig_MissingRequiredField(t *testing.T) {
	agentNoName := `---
description: Missing name
triggers:
  - label: "status:developing"
    event: labeled
role: dev
command: "do stuff"
---
## Agent
`
	dir := setupConfigDir(t, map[string]string{
		"agents/bad.md": agentNoName,
	})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention 'name': %v", err)
	}
	if !strings.Contains(err.Error(), "bad.md") {
		t.Errorf("error should mention filename 'bad.md': %v", err)
	}
}

// Test 3: Empty directory — no agents or workflows, loads fine.
func TestLoadConfig_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(cfg.Agents))
	}
	if len(cfg.Workflows) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(cfg.Workflows))
	}
}

// Test 4: Format error — invalid YAML in frontmatter.
func TestLoadConfig_FormatError(t *testing.T) {
	badYAML := `---
name: [invalid yaml
  broken: {
---
## Bad
`
	dir := setupConfigDir(t, map[string]string{
		"agents/bad.md": badYAML,
	})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for bad YAML")
	}
}

// Test 5: max_retries default value.
func TestLoadConfig_MaxRetriesDefault(t *testing.T) {
	wfNoRetries := `---
name: simple-wf
description: Workflow without max_retries
trigger:
  issue_label: "type:bug"
---
## Simple

` + "```yaml" + `
states:
  open:
    enter_label: "status:open"
    transitions:
      - to: done
        when: "labeled:status:done"
  done:
    enter_label: "status:done"
` + "```" + `
`
	dir := setupConfigDir(t, map[string]string{
		"workflows/simple.md": wfNoRetries,
	})

	cfg, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wf := cfg.Workflows["simple-wf"]
	if wf.MaxRetries != 3 {
		t.Errorf("max_retries = %d, want 3 (default)", wf.MaxRetries)
	}
}

// Test 6: States validation — verify states are properly parsed.
func TestLoadConfig_StatesValidation(t *testing.T) {
	dir := setupConfigDir(t, map[string]string{
		"workflows/feature-dev.md": validWorkflow,
	})

	cfg, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wf := cfg.Workflows["feature-dev"]

	// Check developing state (entry point after humans open an issue).
	developing, ok := wf.States["developing"]
	if !ok {
		t.Fatal("missing 'developing' state")
	}
	if developing.EnterLabel != "status:developing" {
		t.Errorf("developing enter_label = %q, want %q", developing.EnterLabel, "status:developing")
	}
	if developing.Agent != "dev-agent" {
		t.Errorf("developing agent = %q, want %q", developing.Agent, "dev-agent")
	}
	if len(developing.Transitions) < 2 {
		t.Fatalf("developing transitions count = %d, want >= 2 (reviewing + blocked)", len(developing.Transitions))
	}

	// Check reviewing state uses review-agent.
	reviewing, ok := wf.States["reviewing"]
	if !ok {
		t.Fatal("missing 'reviewing' state")
	}
	if reviewing.Agent != "review-agent" {
		t.Errorf("reviewing agent = %q, want %q", reviewing.Agent, "review-agent")
	}
}

// Test 7: Duplicate edge — same (from, to) pair in a workflow.
func TestLoadConfig_DuplicateEdge(t *testing.T) {
	wfDupEdge := `---
name: dup-edge-wf
description: Workflow with duplicate edge
trigger:
  issue_label: "type:dup"
---
## Dup

` + "```yaml" + `
states:
  open:
    enter_label: "status:open"
    transitions:
      - to: done
        when: "labeled:status:done"
      - to: done
        when: "other-condition"
  done:
    enter_label: "status:done"
` + "```" + `
`
	dir := setupConfigDir(t, map[string]string{
		"workflows/dup.md": wfDupEdge,
	})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for duplicate edge")
	}
	if !strings.Contains(err.Error(), "duplicate transition edge") {
		t.Errorf("error should mention 'duplicate transition edge': %v", err)
	}
}

// Test 8: No yaml code block in workflow.
func TestLoadConfig_NoYAMLCodeBlock(t *testing.T) {
	wfNoBlock := `---
name: no-block-wf
description: Missing yaml block
trigger:
  issue_label: "type:noblock"
---
## No Block

Just some markdown without any yaml code block.
`
	dir := setupConfigDir(t, map[string]string{
		"workflows/noblock.md": wfNoBlock,
	})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for missing yaml code block")
	}
	if !strings.Contains(err.Error(), "no yaml code block") {
		t.Errorf("error should mention 'no yaml code block': %v", err)
	}
}

// Test 9: Workflow trigger conflict — two workflows with the same trigger label.
func TestLoadConfig_WorkflowTriggerConflict(t *testing.T) {
	wf1 := `---
name: wf-one
description: First workflow
trigger:
  issue_label: "type:feature"
---
## WF One

` + "```yaml" + `
states:
  start:
    enter_label: "status:start"
    transitions:
      - to: end
        when: done
  end:
    enter_label: "status:end"
` + "```" + `
`
	wf2 := `---
name: wf-two
description: Second workflow
trigger:
  issue_label: "type:feature"
---
## WF Two

` + "```yaml" + `
states:
  begin:
    enter_label: "status:begin"
    transitions:
      - to: finish
        when: done
  finish:
    enter_label: "status:finish"
` + "```" + `
`
	dir := setupConfigDir(t, map[string]string{
		"workflows/wf1.md": wf1,
		"workflows/wf2.md": wf2,
	})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for trigger conflict")
	}
	if !strings.Contains(err.Error(), "trigger conflict") {
		t.Errorf("error should mention 'trigger conflict': %v", err)
	}
}

// Test 10: Agent-label inconsistency — agent trigger label not in any workflow.
func TestLoadConfig_AgentLabelInconsistency(t *testing.T) {
	agent := `---
name: orphan-agent
description: Agent with orphan label
triggers:
  - label: "status:nonexistent"
    event: labeled
role: dev
command: "do stuff"
---
## Orphan
`
	wf := `---
name: simple-wf
description: Simple workflow
trigger:
  issue_label: "type:bug"
---
## Simple

` + "```yaml" + `
states:
  open:
    enter_label: "status:open"
    transitions:
      - to: done
        when: done
  done:
    enter_label: "status:done"
` + "```" + `
`
	dir := setupConfigDir(t, map[string]string{
		"agents/orphan.md":    agent,
		"workflows/simple.md": wf,
	})

	cfg, warnings, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("config should not be nil")
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least 1 warning for agent-label mismatch")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w.Message, "status:nonexistent") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning mentioning 'status:nonexistent', got: %v", warnings)
	}
}

// Test: Config directory does not exist — clear error, no panic.
func TestLoadConfig_DirNotExist(t *testing.T) {
	_, _, err := LoadConfig("/nonexistent/path/to/config")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention 'directory': %v", err)
	}
}

// Test: Runtime default and validation.
func TestLoadConfig_RuntimeDefault(t *testing.T) {
	agentNoRuntime := `---
name: no-runtime
description: Agent without runtime
triggers:
  - label: "status:x"
    event: labeled
role: dev
command: "do stuff"
---
## No Runtime
`
	dir := setupConfigDir(t, map[string]string{
		"agents/no-runtime.md": agentNoRuntime,
	})

	cfg, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agents["no-runtime"].Runtime != "claude-code" {
		t.Errorf("runtime = %q, want %q", cfg.Agents["no-runtime"].Runtime, "claude-code")
	}
}

// Test: Invalid runtime value.
func TestLoadConfig_InvalidRuntime(t *testing.T) {
	agentBadRuntime := `---
name: bad-runtime
description: Agent with bad runtime
triggers:
  - label: "status:x"
    event: labeled
role: dev
runtime: invalid-runtime
command: "do stuff"
---
## Bad Runtime
`
	dir := setupConfigDir(t, map[string]string{
		"agents/bad.md": agentBadRuntime,
	})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for invalid runtime")
	}
	if !strings.Contains(err.Error(), "invalid runtime") {
		t.Errorf("error should mention 'invalid runtime': %v", err)
	}
}

// Test: Auto-add failed state for workflow with back-edges.
func TestLoadConfig_AutoAddFailedState(t *testing.T) {
	wfWithBackEdge := `---
name: retry-wf
description: Workflow with retry cycle
trigger:
  issue_label: "type:retry"
---
## Retry

` + "```yaml" + `
states:
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: testing
        when: "labeled:status:testing"
  testing:
    enter_label: "status:testing"
    agent: test-agent
    transitions:
      - to: done
        when: "labeled:status:done"
      - to: developing
        when: "labeled:status:developing"
  done:
    enter_label: "status:done"
` + "```" + `
`
	dir := setupConfigDir(t, map[string]string{
		"workflows/retry.md": wfWithBackEdge,
	})

	cfg, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wf := cfg.Workflows["retry-wf"]
	failedState, ok := wf.States[StateNameFailed]
	if !ok {
		t.Fatal("expected auto-added 'failed' state")
	}
	if failedState.EnterLabel != LabelFailed {
		t.Errorf("failed state enter_label = %q, want %q", failedState.EnterLabel, LabelFailed)
	}
}

func TestLoadConfig_CodexPolicyNormalize(t *testing.T) {
	agent := `---
name: codex-agent
description: Codex agent
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: codex
policy:
  sandbox: danger-full-access
  approval: on-request
  model: gpt-5.4
  timeout: 45m
prompt: |
  implement issue {{.Issue.Number}}
command: |
  codex exec "compat"
---
## Agent
`
	dir := setupConfigDir(t, map[string]string{"agents/codex.md": agent, "workflows/feature-dev.md": validWorkflow})
	cfg, warnings, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	got := cfg.Agents["codex-agent"]
	if got.Runtime != RuntimeCodexExec {
		t.Fatalf("runtime = %q", got.Runtime)
	}
	if got.Policy.Model != "gpt-5.4" || got.Policy.Approval != "on-request" || got.Policy.Sandbox != "danger-full-access" {
		t.Fatalf("unexpected policy: %+v", got.Policy)
	}
	if got.Timeout != 45*time.Minute {
		t.Fatalf("timeout = %s", got.Timeout)
	}
}

func TestLoadConfig_UnsupportedPolicyMatrix(t *testing.T) {
	agent := `---
name: bad-agent
description: Bad agent
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: claude-code
policy:
  sandbox: read-only
  approval: on-request
command: "claude -p 'do stuff'"
---
## Agent
`
	dir := setupConfigDir(t, map[string]string{"agents/bad.md": agent})
	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected policy validation error")
	}
	if !strings.Contains(err.Error(), "unsupported policy.approval") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfig_CodexAppServerPolicyAccepted(t *testing.T) {
	agent := `---
name: future-agent
description: Future agent
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: codex-appserver
policy:
  sandbox: workspace-write
  approval: via-approver
prompt: |
  implement issue {{.Issue.Number}}
---
## Agent
`
	dir := setupConfigDir(t, map[string]string{"agents/future.md": agent, "workflows/feature-dev.md": validWorkflow})
	cfg, warnings, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if cfg.Agents["future-agent"].Runtime != RuntimeCodexServer {
		t.Fatalf("runtime = %q", cfg.Agents["future-agent"].Runtime)
	}
}

func TestLoadConfig_OutputContractSchemaPathResolved(t *testing.T) {
	agent := `---
name: contract-agent
description: Structured output agent
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: codex
prompt: |
  emit json
output_contract:
  schema_file: schemas/result.json
---
## Agent
`
	schema := `{
  "type": "object",
  "required": ["status"],
  "properties": {
    "status": {"type": "string"}
  }
}
`
	dir := setupConfigDir(t, map[string]string{
		"agents/contract-agent.md":   agent,
		"agents/schemas/result.json": schema,
		"workflows/feature-dev.md":   validWorkflow,
	})

	cfg, warnings, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	got := cfg.Agents["contract-agent"].OutputContractSchemaPath()
	want := filepath.Join(dir, "agents", "schemas", "result.json")
	if got != want {
		t.Fatalf("schema path = %q, want %q", got, want)
	}
}

func TestLoadConfig_OutputContractSchemaFileMissing(t *testing.T) {
	agent := `---
name: contract-agent
description: Structured output agent
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: codex
prompt: |
  emit json
output_contract:
  schema_file: schemas/missing.json
---
## Agent
`
	dir := setupConfigDir(t, map[string]string{"agents/contract-agent.md": agent})

	_, _, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected schema file error")
	}
	if !strings.Contains(err.Error(), "output_contract.schema_file") {
		t.Fatalf("unexpected error: %v", err)
	}
}
