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

// AgentConfig defines an agent loaded from .github/workbuddy/agents/*.md.
//
// The new format (issue #204 batch 2) stores agent metadata in YAML frontmatter
// and uses the markdown body as the prompt template. The body lives on the
// `Prompt` field (yaml tag `-`) — it is populated by the loader from the bytes
// after the closing `---`, never parsed from YAML. Frontmatter no longer
// accepts a `prompt:` field.
type AgentConfig struct {
	Name           string               `yaml:"name"`
	Description    string               `yaml:"description"`
	Triggers       []TriggerRule        `yaml:"triggers"`
	Role           string               `yaml:"role"`
	Runtime        string               `yaml:"runtime"`
	Command        string               `yaml:"command"`
	Context        []string             `yaml:"context"`
	Prompt         string               `yaml:"-"` // markdown body, populated by the loader
	Policy         PolicyConfig         `yaml:"policy"`
	Permissions    PermissionsConfig    `yaml:"permissions"`
	OutputContract OutputContractConfig `yaml:"output_contract"`
	Timeout        time.Duration        `yaml:"timeout"`
	// DevContainerImage names the dev container image AgentM should run
	// inside when the runtime is `agentm`. workbuddy passes the value
	// through to the AgentM subprocess as the env var
	// AGENTM_AGENT_ENV_IMAGE; AgentM is responsible for the actual
	// sandbox dispatch (workbuddy does not talk to agent-env directly).
	// See docs/planned/agentm-runtime.md for the invocation contract and
	// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 2) for design
	// context. Optional; only meaningful for `runtime: agentm`. Setting
	// it on other runtimes emits a config warning (not an error) so a
	// per-agent override can be staged ahead of an upcoming migration.
	DevContainerImage string `yaml:"dev_container_image,omitempty"`
	// Scenario names the AgentM scenario to load (`agentm --scenario X`).
	// Only meaningful for `runtime: agentm`. When empty, AgentM falls back
	// to its own default scenario (general_purpose). See
	// docs/planned/agentm-runtime.md for the CLI invocation contract.
	Scenario string `yaml:"scenario,omitempty"`
	// Extensions lists extra AgentM atoms to mount on top of the scenario,
	// emitted as repeated `-e module[:json]` CLI flags. The system prompt
	// is just an extension entry (e.g. module
	// `agentm.extensions.builtin.system_prompt` with a `prompt`/`prompt_file`
	// config) — no special-casing. Only meaningful for `runtime: agentm`.
	Extensions []AgentExtension `yaml:"extensions,omitempty"`
	SourcePath string           `yaml:"-"`
}

// NOTE: Prompt and SourcePath have no JSON tags on purpose. AgentConfig is
// serialized to JSON when the coordinator ships per-repo agent config to the
// worker over the dispatch wire (workerclient.Task.Agent / ADR 2026-06-06 §2).
// Prompt carries the agent's markdown instructions, so adding `json:"-"` here
// would silently drop the agent's prompt from every dispatched task.

// DeepCopy returns a copy of the AgentConfig with all reference-typed fields
// (slices and nested maps) independently allocated, so mutating the returned
// value cannot affect the original. The nested struct fields (Policy,
// Permissions, OutputContract) hold only scalars and are copied
// by the shallow struct assignment. Used by the coordinator dispatch path to
// hand out an isolated copy of the live per-repo registration config.
func (a *AgentConfig) DeepCopy() *AgentConfig {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Triggers != nil {
		cp.Triggers = append([]TriggerRule(nil), a.Triggers...)
	}
	if a.Context != nil {
		cp.Context = append([]string(nil), a.Context...)
	}
	if a.Extensions != nil {
		cp.Extensions = make([]AgentExtension, len(a.Extensions))
		for i, ext := range a.Extensions {
			cp.Extensions[i] = ext
			if ext.Config != nil {
				cfg := make(map[string]any, len(ext.Config))
				for k, v := range ext.Config {
					cfg[k] = v
				}
				cp.Extensions[i].Config = cfg
			}
		}
	}
	return &cp
}

// AgentExtension is one `-e module[:json]` pair passed to the AgentM CLI.
// Module is a dotted Python import path; Config is serialized to JSON and
// appended after a colon when non-empty.
type AgentExtension struct {
	Module string         `yaml:"module"`
	Config map[string]any `yaml:"config,omitempty"`
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
