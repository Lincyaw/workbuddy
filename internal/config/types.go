package config

import "time"

// Well-known state names.
const (
	StateNameFailed = "failed"
	LabelFailed     = "status:failed"
	StateModeReview = "review"
	StateModeSynth  = "synthesize"
)

// GlobalConfig is the top-level configuration loaded from config.yaml.
type GlobalConfig struct {
	Repo         string        `yaml:"repo"`
	Environment  string        `yaml:"environment"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Port         int           `yaml:"port"`
}

// OperatorConfig controls the event-driven self-healing detector and incident dispatcher.
type OperatorConfig struct {
	Enabled       bool          `yaml:"enabled"`
	CheckInterval time.Duration `yaml:"check_interval"`
	DedupWindow   time.Duration `yaml:"dedup_window"`
	InboxDir      string        `yaml:"inbox_dir"`
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
//
// The new format (issue #204 batch 2) stores agent metadata in YAML frontmatter
// and uses the markdown body as the prompt template. The body lives on the
// `Prompt` field (yaml tag `-`) — it is populated by the loader from the bytes
// after the closing `---`, never parsed from YAML. Frontmatter no longer
// accepts a `prompt:` field.
type AgentConfig struct {
	Name           string                    `yaml:"name"`
	Description    string                    `yaml:"description"`
	Triggers       []TriggerRule             `yaml:"triggers"`
	Role           string                    `yaml:"role"`
	Runner         string                    `yaml:"runner"`
	Runtime        string                    `yaml:"runtime"`
	Command        string                    `yaml:"command"`
	Context        []string                  `yaml:"context"`
	Prompt         string                    `yaml:"-"` // markdown body, populated by the loader
	Policy         PolicyConfig              `yaml:"policy"`
	Permissions    PermissionsConfig         `yaml:"permissions"`
	GitHubActions  GitHubActionsRunnerConfig `yaml:"github_actions"`
	OutputContract OutputContractConfig      `yaml:"output_contract"`
	Timeout        time.Duration             `yaml:"timeout"`
	SourcePath     string                    `yaml:"-"`
}

// TriggerRule defines when an agent is activated. The agent references workflow
// state names symbolically; the actual issue-label string is owned only by the
// workflow's State.EnterLabel.
type TriggerRule struct {
	State string `yaml:"state"`
	Event string `yaml:"event"`
}

// WorkflowConfig defines a workflow loaded from .github/workbuddy/workflows/*.md.
type WorkflowConfig struct {
	Name        string          `yaml:"name"`
	Description string          `yaml:"description"`
	Trigger     WorkflowTrigger `yaml:"trigger"`
	MaxRetries  int             `yaml:"max_retries"`
	// MaxReviewCycles caps the number of dev↔review round-trips
	// (developing→reviewing→developing transitions) the orchestrator will
	// dispatch automatically before flagging the issue as needing human review.
	// Default: 3. Set to 0 in YAML to inherit the default.
	MaxReviewCycles int               `yaml:"max_review_cycles"`
	States          map[string]*State // parsed from embedded YAML code block
}

// WorkflowTrigger defines what issue label activates this workflow.
type WorkflowTrigger struct {
	IssueLabel string `yaml:"issue_label"`
}

// JoinConfig controls how a state's sibling tasks converge.
//
// Legacy scalar YAML such as `join: all_passed` still decodes via State's
// custom UnmarshalYAML implementation. Rollout joins use `strategy: rollouts`
// with optional `min_successes`.
type JoinConfig struct {
	Strategy     string `yaml:"strategy,omitempty"`
	MinSuccesses int    `yaml:"min_successes,omitempty"`
}

// State defines a single state in the workflow state machine. Transitions are
// modeled as a label→target-state-name map: the key is the issue label whose
// arrival drives the transition, the value is the target state name. Empty
// map (or nil) marks a terminal state.
type State struct {
	EnterLabel  string            `yaml:"enter_label"`
	Agent       string            `yaml:"agent,omitempty"`
	Agents      []string          `yaml:"agents,omitempty"`
	Mode        string            `yaml:"mode,omitempty"`
	Join        JoinConfig        `yaml:"join,omitempty"`
	Rollouts    int               `yaml:"rollouts,omitempty"`
	Transitions map[string]string `yaml:"transitions"`
}

// StatesBlock is the wrapper for parsing the YAML code block in workflow markdown.
type StatesBlock struct {
	States map[string]*State `yaml:"states"`
}

// WorkerConfig holds worker-level configuration knobs.
type WorkerConfig struct {
	StaleInference StaleInferenceConfig `yaml:"stale_inference"`
}

// StaleInferenceConfig controls the stale inference watchdog that kills
// hung agent processes when no session output is produced for too long.
type StaleInferenceConfig struct {
	Enabled              *bool         `yaml:"enabled"`
	IdleThreshold        time.Duration `yaml:"idle_threshold"`         // default 10m
	CheckInterval        time.Duration `yaml:"check_interval"`         // default 30s
	CompletedGracePeriod time.Duration `yaml:"completed_grace_period"` // default 60s
}

// StaleInferenceEnabled returns whether the watchdog is enabled,
// defaulting to true when the field is nil.
func (c *StaleInferenceConfig) StaleInferenceEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// FullConfig holds all loaded configuration.
type FullConfig struct {
	Global        GlobalConfig   `yaml:",inline"`
	Operator      OperatorConfig `yaml:"operator"`
	Worker        WorkerConfig   `yaml:"worker"`
	Agents        map[string]*AgentConfig
	Workflows     map[string]*WorkflowConfig
	Notifications NotificationsConfig `yaml:"notifications"`
}
