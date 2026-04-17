package config

import "time"

// Well-known state names.
const (
	StateNameFailed = "failed"
	LabelFailed     = "status:failed"
)

// GlobalConfig is the top-level configuration loaded from config.yaml.
type GlobalConfig struct {
	Repo         string        `yaml:"repo"`
	Environment  string        `yaml:"environment"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Port         int           `yaml:"port"`
}

// OperatorConfig controls the local incident-operator dispatcher.
type OperatorConfig struct {
	Enabled bool `yaml:"enabled"`
}

// NotificationsConfig controls external notification routing.
type NotificationsConfig struct {
	Enabled      bool          `yaml:"enabled"`
	InstanceName string        `yaml:"instance_name"`
	DedupWindow  time.Duration `yaml:"dedup_window"`
	BatchWindow  time.Duration `yaml:"batch_window"`
	Success      bool          `yaml:"success"`

	Slack    *WebhookChannelConfig  `yaml:"slack"`
	Feishu   *WebhookChannelConfig  `yaml:"feishu"`
	Telegram *TelegramChannelConfig `yaml:"telegram"`
	SMTP     *SMTPChannelConfig     `yaml:"smtp"`
}

// WebhookChannelConfig defines webhook delivery options for Slack/Feishu.
type WebhookChannelConfig struct {
	Enabled       bool   `yaml:"enabled"`
	WebhookURLEnv string `yaml:"webhook_url_env"`
}

// TelegramChannelConfig defines Telegram Bot API delivery options.
type TelegramChannelConfig struct {
	Enabled     bool   `yaml:"enabled"`
	BotTokenEnv string `yaml:"bot_token_env"`
	ChatIDEnv   string `yaml:"chat_id_env"`
	ParseMode   string `yaml:"parse_mode"`
}

// SMTPChannelConfig defines SMTP/SMTPS delivery options.
type SMTPChannelConfig struct {
	Enabled     bool   `yaml:"enabled"`
	HostEnv     string `yaml:"host_env"`
	PortEnv     string `yaml:"port_env"`
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`
	FromEnv     string `yaml:"from_env"`
	ToEnv       string `yaml:"to_env"`
}

// PolicyConfig defines runtime-neutral execution policy knobs.
type PolicyConfig struct {
	Sandbox  string        `yaml:"sandbox"`
	Approval string        `yaml:"approval"`
	Model    string        `yaml:"model"`
	Timeout  time.Duration `yaml:"timeout"`
}

// PermissionsConfig controls subprocess capability boundaries used by the launcher.
type PermissionsConfig struct {
	GitHub    GitHubPermissionsConfig     `yaml:"github"`
	FS        FileSystemPermissionsConfig `yaml:"fs"`
	Resources ResourceLimitsConfig        `yaml:"resources"`
}

// GitHubPermissionsConfig identifies which token environment variable should be
// treated as the scoped PAT for a subprocess.
type GitHubPermissionsConfig struct {
	Token string `yaml:"token"`
}

// FileSystemPermissionsConfig declares filesystem write scope. Enforcement is
// deferred to v0.5.0; v0.4.0 keeps schema support and event emission.
type FileSystemPermissionsConfig struct {
	Write string `yaml:"write"`
}

// ResourceLimitsConfig holds optional resource caps for future enforcement.
// Enforcement is deferred to v0.5.0.
type ResourceLimitsConfig struct {
	MaxMemoryMB   int `yaml:"max_memory_mb"`
	MaxCPUPercent int `yaml:"max_cpu_percent"`
}

// OutputContractConfig describes a structured-output contract for an agent.
type OutputContractConfig struct {
	SchemaFile string `yaml:"schema_file"`
}

// GitHubActionsRunnerConfig defines the workflow and polling knobs for the
// remote GitHub Actions runner.
type GitHubActionsRunnerConfig struct {
	Workflow     string        `yaml:"workflow"`
	Ref          string        `yaml:"ref"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// AgentConfig defines an agent loaded from .github/workbuddy/agents/*.md.
type AgentConfig struct {
	Name           string                    `yaml:"name"`
	Description    string                    `yaml:"description"`
	Triggers       []TriggerRule             `yaml:"triggers"`
	Role           string                    `yaml:"role"`
	Runner         string                    `yaml:"runner"`
	Runtime        string                    `yaml:"runtime"`
	Command        string                    `yaml:"command"`
	Prompt         string                    `yaml:"prompt"`
	Policy         PolicyConfig              `yaml:"policy"`
	Permissions    PermissionsConfig         `yaml:"permissions"`
	GitHubActions  GitHubActionsRunnerConfig `yaml:"github_actions"`
	OutputContract OutputContractConfig      `yaml:"output_contract"`
	Timeout        time.Duration             `yaml:"timeout"`
	SourcePath     string                    `yaml:"-"`
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
	Agents      []string     `yaml:"agents,omitempty"`
	Join        string       `yaml:"join,omitempty"`
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
	Global        GlobalConfig   `yaml:",inline"`
	Operator      OperatorConfig `yaml:"operator"`
	Agents        map[string]*AgentConfig
	Workflows     map[string]*WorkflowConfig
	Notifications NotificationsConfig `yaml:"notifications"`
}
