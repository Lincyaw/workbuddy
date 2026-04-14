package config

import "time"

// GlobalConfig is the top-level configuration loaded from config.yaml.
type GlobalConfig struct {
	Repo         string        `yaml:"repo"`
	Environment  string        `yaml:"environment"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Port         int           `yaml:"port"`
}

// AgentConfig defines an agent loaded from .github/workbuddy/agents/*.md.
type AgentConfig struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Triggers    []TriggerRule `yaml:"triggers"`
	Role        string        `yaml:"role"`
	Runtime     string        `yaml:"runtime"` // "claude-code" (default) or "codex"
	Command     string        `yaml:"command"`
	Timeout     time.Duration `yaml:"timeout"`
}

// TriggerRule defines when an agent is activated.
type TriggerRule struct {
	Label string `yaml:"label"`
	Event string `yaml:"event"`
}

// WorkflowConfig defines a workflow loaded from .github/workbuddy/workflows/*.md.
type WorkflowConfig struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Trigger     WorkflowTrigger   `yaml:"trigger"`
	MaxRetries  int               `yaml:"max_retries"`
	States      map[string]*State // parsed from embedded YAML code block
}

// WorkflowTrigger defines what issue label activates this workflow.
type WorkflowTrigger struct {
	IssueLabel string `yaml:"issue_label"`
}

// State defines a single state in the workflow state machine.
type State struct {
	EnterLabel  string       `yaml:"enter_label"`
	Agent       string       `yaml:"agent,omitempty"`
	Action      string       `yaml:"action,omitempty"`
	Transitions []Transition `yaml:"transitions"`
}

// Transition defines a possible state change.
type Transition struct {
	To   string `yaml:"to"`
	When string `yaml:"when"`
}

// StatesBlock is the wrapper for parsing the YAML code block in workflow markdown.
type StatesBlock struct {
	States map[string]*State `yaml:"states"`
}

// FullConfig holds all loaded configuration.
type FullConfig struct {
	Global    GlobalConfig
	Agents    map[string]*AgentConfig
	Workflows map[string]*WorkflowConfig
}
