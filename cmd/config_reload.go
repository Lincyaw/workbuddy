package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/notifier"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/fsnotify/fsnotify"
)

const configReloadDebounce = 200 * time.Millisecond

type notifierRuntime struct {
	rootCtx context.Context
	bus     *alertbus.Bus
	taskHub *tasknotify.Hub
	logger  *eventlog.EventLogger

	mu     sync.Mutex
	cancel context.CancelFunc
}

func newNotifierRuntime(rootCtx context.Context, cfg config.NotificationsConfig, bus *alertbus.Bus, taskHub *tasknotify.Hub, logger *eventlog.EventLogger) (*notifierRuntime, error) {
	r := &notifierRuntime{
		rootCtx: rootCtx,
		bus:     bus,
		taskHub: taskHub,
		logger:  logger,
	}
	if err := r.Apply(cfg); err != nil {
		return nil, err
	}
	return r, nil
}

func validateNotificationsConfig(cfg config.NotificationsConfig, bus *alertbus.Bus, taskHub *tasknotify.Hub, logger *eventlog.EventLogger) error {
	_, err := notifier.New(cfg, bus, taskHub, logger)
	return err
}

func (r *notifierRuntime) Apply(cfg config.NotificationsConfig) error {
	next, err := notifier.New(cfg, r.bus, r.taskHub, r.logger)
	if err != nil {
		return err
	}
	childCtx, cancel := context.WithCancel(r.rootCtx)
	next.Start(childCtx)

	r.mu.Lock()
	prevCancel := r.cancel
	r.cancel = cancel
	r.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
	}
	return nil
}

type configReloadSummary struct {
	Trigger       string    `json:"trigger"`
	Repo          string    `json:"repo"`
	PollInterval  string    `json:"poll_interval"`
	AgentCount    int       `json:"agent_count"`
	WorkflowCount int       `json:"workflow_count"`
	Changed       []string  `json:"changed"`
	Warnings      []string  `json:"warnings,omitempty"`
	ReloadedAt    time.Time `json:"reloaded_at"`
}

type coordinatorConfigRuntime struct {
	configDir string
	store     *store.Store
	eventlog  *eventlog.EventLogger
	pollers   *pollerManager
	registry  *registry.Registry
	notifier  *notifierRuntime
	taskHub   *tasknotify.Hub
	alertBus  *alertbus.Bus

	mu      sync.RWMutex
	current *config.FullConfig
}

func newCoordinatorConfigRuntime(configDir string, current *config.FullConfig, st *store.Store, evlog *eventlog.EventLogger, pollers *pollerManager, reg *registry.Registry, notifierSvc *notifierRuntime, alertBus *alertbus.Bus, taskHub *tasknotify.Hub) *coordinatorConfigRuntime {
	return &coordinatorConfigRuntime{
		configDir: strings.TrimSpace(configDir),
		store:     st,
		eventlog:  evlog,
		pollers:   pollers,
		registry:  reg,
		notifier:  notifierSvc,
		taskHub:   taskHub,
		alertBus:  alertBus,
		current:   cloneFullConfig(current),
	}
}

func (r *coordinatorConfigRuntime) Current() *config.FullConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneFullConfig(r.current)
}

func (r *coordinatorConfigRuntime) Reload(trigger string) (*configReloadSummary, error) {
	current := r.Current()

	cfg, warnings, err := config.LoadConfig(r.configDir)
	if err != nil {
		return nil, r.failReload(current, trigger, fmt.Errorf("load config: %w", err))
	}
	for _, warning := range warnings {
		log.Printf("[coordinator] config warning during reload: %s", warning)
	}

	cfg, warningMessages, err := normalizeReloadCandidate(current, cfg, warnings)
	if err != nil {
		return nil, r.failReload(current, trigger, err)
	}

	if err := validateNotificationsConfig(cfg.Notifications, r.alertBus, r.taskHub, r.eventlog); err != nil {
		return nil, r.failReload(current, trigger, fmt.Errorf("validate notifications: %w", err))
	}

	rec, err := buildRepoRegistrationRecord(buildRepoRegistrationPayload(cfg))
	if err != nil {
		return nil, r.failReload(current, trigger, fmt.Errorf("build registration: %w", err))
	}

	var prevRec *store.RepoRegistrationRecord
	if current != nil && strings.TrimSpace(current.Global.Repo) != "" {
		prevRec, err = r.store.GetRepoRegistration(current.Global.Repo)
		if err != nil {
			return nil, r.failReload(current, trigger, fmt.Errorf("lookup current registration: %w", err))
		}
	}

	if err := r.store.UpsertRepoRegistration(rec); err != nil {
		return nil, r.failReload(current, trigger, fmt.Errorf("persist registration: %w", err))
	}
	if err := r.pollers.StartOrUpdate(rec); err != nil {
		if rollbackErr := r.rollbackRegistration(prevRec, rec.Repo); rollbackErr != nil {
			return nil, r.failReload(current, trigger, fmt.Errorf("start updated runtime: %v (rollback failed: %v)", err, rollbackErr))
		}
		return nil, r.failReload(current, trigger, fmt.Errorf("start updated runtime: %w", err))
	}
	if r.registry != nil {
		r.registry.SetPollInterval(cfg.Global.PollInterval)
	}
	if r.notifier != nil {
		if err := r.notifier.Apply(cfg.Notifications); err != nil {
			if rollbackErr := r.rollbackRegistration(prevRec, rec.Repo); rollbackErr != nil {
				return nil, r.failReload(current, trigger, fmt.Errorf("apply notifications: %v (rollback failed: %v)", err, rollbackErr))
			}
			if current != nil && r.registry != nil {
				r.registry.SetPollInterval(current.Global.PollInterval)
			}
			return nil, r.failReload(current, trigger, fmt.Errorf("apply notifications: %w", err))
		}
	}

	summary := &configReloadSummary{
		Trigger:       strings.TrimSpace(trigger),
		Repo:          cfg.Global.Repo,
		PollInterval:  cfg.Global.PollInterval.String(),
		AgentCount:    len(cfg.Agents),
		WorkflowCount: len(cfg.Workflows),
		Changed:       diffConfigReload(current, cfg),
		Warnings:      warningMessages,
		ReloadedAt:    time.Now().UTC(),
	}

	r.mu.Lock()
	r.current = cloneFullConfig(cfg)
	r.mu.Unlock()

	r.eventlog.Log(eventlog.TypeConfigReloaded, cfg.Global.Repo, 0, summary)
	return summary, nil
}

func (r *coordinatorConfigRuntime) failReload(current *config.FullConfig, trigger string, err error) error {
	repo := ""
	if current != nil {
		repo = current.Global.Repo
	}
	log.Printf("[coordinator] config reload failed (%s): %v", trigger, err)
	r.eventlog.Log(eventlog.TypeConfigReloadFailed, repo, 0, map[string]any{
		"trigger": trigger,
		"error":   err.Error(),
	})
	return err
}

func (r *coordinatorConfigRuntime) rollbackRegistration(prev *store.RepoRegistrationRecord, repo string) error {
	if prev == nil {
		if err := r.pollers.Deregister(repo); err != nil {
			return err
		}
		return nil
	}
	if err := r.store.UpsertRepoRegistration(*prev); err != nil {
		return err
	}
	return r.pollers.StartOrUpdate(*prev)
}

func normalizeReloadCandidate(current *config.FullConfig, candidate *config.FullConfig, warnings []config.Warning) (*config.FullConfig, []string, error) {
	out := cloneFullConfig(candidate)
	if out == nil {
		return nil, nil, fmt.Errorf("empty config candidate")
	}
	if out.Global.PollInterval <= 0 {
		if current != nil && current.Global.PollInterval > 0 {
			out.Global.PollInterval = current.Global.PollInterval
		} else {
			out.Global.PollInterval = defaultPollInterval
		}
	}
	warningMessages := make([]string, 0, len(warnings)+1)
	for _, warning := range warnings {
		warningMessages = append(warningMessages, warning.Message)
	}

	if current != nil {
		if strings.TrimSpace(out.Global.Repo) == "" {
			out.Global.Repo = current.Global.Repo
		}
		if out.Global.Repo != current.Global.Repo {
			return nil, nil, fmt.Errorf("repo change from %q to %q requires restart", current.Global.Repo, out.Global.Repo)
		}
		if out.Global.Port != 0 && out.Global.Port != current.Global.Port {
			warningMessages = append(warningMessages, fmt.Sprintf("port change from %d to %d ignored until restart", current.Global.Port, out.Global.Port))
		}
		out.Global.Port = current.Global.Port
	}
	if strings.TrimSpace(out.Global.Repo) == "" {
		return nil, nil, fmt.Errorf("config must specify repo")
	}
	return out, warningMessages, nil
}

func diffConfigReload(prev, next *config.FullConfig) []string {
	if next == nil {
		return nil
	}
	if prev == nil {
		return []string{"initial_load"}
	}
	var changed []string
	if prev.Global.PollInterval != next.Global.PollInterval {
		changed = append(changed, "poll_interval")
	}
	if !reflect.DeepEqual(prev.Notifications, next.Notifications) {
		changed = append(changed, "notifications")
	}
	if agentsChanged(prev.Agents, next.Agents) {
		changed = append(changed, "agents")
	}
	if workflowsChanged(prev.Workflows, next.Workflows) {
		changed = append(changed, "workflows")
	}
	if len(changed) == 0 {
		changed = append(changed, "no_effective_changes")
	}
	return changed
}

func agentsChanged(prev, next map[string]*config.AgentConfig) bool {
	return marshalComparableAgents(prev) != marshalComparableAgents(next)
}

func workflowsChanged(prev, next map[string]*config.WorkflowConfig) bool {
	return marshalJSON(marshalComparableWorkflows(prev)) != marshalJSON(marshalComparableWorkflows(next))
}

func marshalComparableAgents(in map[string]*config.AgentConfig) string {
	type comparableAgent struct {
		Name           string                           `json:"name"`
		Description    string                           `json:"description"`
		Triggers       []config.TriggerRule             `json:"triggers"`
		Role           string                           `json:"role"`
		Runner         string                           `json:"runner"`
		Runtime        string                           `json:"runtime"`
		Command        string                           `json:"command"`
		Prompt         string                           `json:"prompt"`
		Policy         config.PolicyConfig              `json:"policy"`
		Permissions    config.PermissionsConfig         `json:"permissions"`
		GitHubActions  config.GitHubActionsRunnerConfig `json:"github_actions"`
		OutputContract config.OutputContractConfig      `json:"output_contract"`
		Timeout        time.Duration                    `json:"timeout"`
	}
	out := make(map[string]comparableAgent, len(in))
	for name, agent := range in {
		if agent == nil {
			continue
		}
		out[name] = comparableAgent{
			Name:           agent.Name,
			Description:    agent.Description,
			Triggers:       append([]config.TriggerRule(nil), agent.Triggers...),
			Role:           agent.Role,
			Runner:         agent.Runner,
			Runtime:        agent.Runtime,
			Command:        agent.Command,
			Prompt:         agent.Prompt,
			Policy:         agent.Policy,
			Permissions:    agent.Permissions,
			GitHubActions:  agent.GitHubActions,
			OutputContract: agent.OutputContract,
			Timeout:        agent.Timeout,
		}
	}
	return marshalJSON(out)
}

func marshalComparableWorkflows(in map[string]*config.WorkflowConfig) map[string]config.WorkflowConfig {
	out := make(map[string]config.WorkflowConfig, len(in))
	for name, workflow := range in {
		if workflow == nil {
			continue
		}
		out[name] = *workflow
	}
	return out
}

func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"marshal_error":%q}`, err.Error())
	}
	return string(data)
}

func cloneFullConfig(in *config.FullConfig) *config.FullConfig {
	if in == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out config.FullConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return in
	}
	return &out
}

func startCoordinatorConfigWatcher(ctx context.Context, configDir string, runtime *coordinatorConfigRuntime) error {
	if runtime == nil || strings.TrimSpace(configDir) == "" {
		return nil
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create config watcher: %w", err)
	}

	watchDirs := []string{
		configDir,
		filepath.Join(configDir, "agents"),
		filepath.Join(configDir, "workflows"),
	}
	for _, dir := range watchDirs {
		info, err := os.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			_ = watcher.Close()
			return fmt.Errorf("stat watcher dir %s: %w", dir, err)
		}
		if !info.IsDir() {
			continue
		}
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return fmt.Errorf("watch %s: %w", dir, err)
		}
	}

	go func() {
		defer func() { _ = watcher.Close() }()

		var (
			timer   *time.Timer
			timerCh <-chan time.Time
		)
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("[coordinator] config watcher error: %v", err)
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if !isReloadableConfigEvent(configDir, event) {
					continue
				}
				log.Printf("[coordinator] detected config change: %s %s", event.Op.String(), event.Name)
				if timer == nil {
					timer = time.NewTimer(configReloadDebounce)
					timerCh = timer.C
					continue
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(configReloadDebounce)
			case <-timerCh:
				timer = nil
				timerCh = nil
				if _, err := runtime.Reload("file_watch"); err != nil {
					continue
				}
			}
		}
	}()
	return nil
}

func isReloadableConfigEvent(configDir string, event fsnotify.Event) bool {
	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	root := filepath.Clean(configDir)
	path := filepath.Clean(event.Name)
	if filepath.Dir(path) == root && filepath.Base(path) == "config.yaml" {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	sep := string(os.PathSeparator)
	return (strings.HasPrefix(rel, "agents"+sep) || strings.HasPrefix(rel, "workflows"+sep)) && strings.HasSuffix(rel, ".md")
}
