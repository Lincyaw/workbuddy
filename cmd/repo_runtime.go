package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type repoRegistrationPayload struct {
	Repo        string                   `json:"repo"`
	Environment string                   `json:"environment,omitempty"`
	Agents      []*config.AgentConfig    `json:"agents"`
	Workflows   []*config.WorkflowConfig `json:"workflows"`
}

type repoRuntime struct {
	registration          store.RepoRegistrationRecord
	config                *config.FullConfig
	stateMachine          *statemachine.StateMachine
	depResolver           *dependency.Resolver
	dispatchCh            chan statemachine.DispatchRequest
	cancel                context.CancelFunc
	done                  chan struct{}
	depsResolvedThisCycle bool
	depGraphVersion       int64
}

type repoStatus struct {
	Registration store.RepoRegistrationRecord
	PollerStatus string
}

type pollerManager struct {
	rootCtx      context.Context
	store        *store.Store
	registry     *registry.Registry
	eventlog     *eventlog.EventLogger
	alertBus     *alertbus.Bus
	ghReader     poller.GHReader
	reporter     *reporter.Reporter
	repoRoot     string
	pollInterval time.Duration

	mu       sync.RWMutex
	runtimes map[string]*repoRuntime
	events   chan poller.ChangeEvent
}

func newPollerManager(ctx context.Context, st *store.Store, reg *registry.Registry, evlog *eventlog.EventLogger, ab *alertbus.Bus, ghReader poller.GHReader, rep *reporter.Reporter, repoRoot string, pollInterval time.Duration) *pollerManager {
	pm := &pollerManager{
		rootCtx:      ctx,
		store:        st,
		registry:     reg,
		eventlog:     evlog,
		alertBus:     ab,
		ghReader:     ghReader,
		reporter:     rep,
		repoRoot:     repoRoot,
		pollInterval: pollInterval,
		runtimes:     make(map[string]*repoRuntime),
		events:       make(chan poller.ChangeEvent, 256),
	}
	go pm.run()
	return pm
}

func buildRepoRegistrationPayload(cfg *config.FullConfig) *repoRegistrationPayload {
	payload := &repoRegistrationPayload{
		Repo:        strings.TrimSpace(cfg.Global.Repo),
		Environment: strings.TrimSpace(cfg.Global.Environment),
		Agents:      make([]*config.AgentConfig, 0, len(cfg.Agents)),
		Workflows:   make([]*config.WorkflowConfig, 0, len(cfg.Workflows)),
	}
	for _, agent := range cfg.Agents {
		agentCopy := *agent
		agentCopy.SourcePath = ""
		payload.Agents = append(payload.Agents, &agentCopy)
	}
	for _, workflow := range cfg.Workflows {
		workflowCopy := *workflow
		payload.Workflows = append(payload.Workflows, &workflowCopy)
	}
	return payload
}

func buildRepoRegistrationRecord(payload *repoRegistrationPayload) (store.RepoRegistrationRecord, error) {
	if payload == nil {
		return store.RepoRegistrationRecord{}, fmt.Errorf("repo registration payload is required")
	}

	candidate := &repoRegistrationPayload{
		Repo:        strings.TrimSpace(payload.Repo),
		Environment: strings.TrimSpace(payload.Environment),
		Agents:      payload.Agents,
		Workflows:   payload.Workflows,
	}
	configJSON, err := json.Marshal(candidate)
	if err != nil {
		return store.RepoRegistrationRecord{}, fmt.Errorf("marshal registration config: %w", err)
	}

	rec := store.RepoRegistrationRecord{
		Repo:        candidate.Repo,
		Environment: candidate.Environment,
		Status:      "active",
		ConfigJSON:  string(configJSON),
	}
	cfg, err := decodeRepoRegistrationConfig(rec)
	if err != nil {
		return store.RepoRegistrationRecord{}, err
	}

	normalizedPayload := buildRepoRegistrationPayload(cfg)
	configJSON, err = json.Marshal(normalizedPayload)
	if err != nil {
		return store.RepoRegistrationRecord{}, fmt.Errorf("marshal normalized registration config: %w", err)
	}
	rec.Environment = normalizedPayload.Environment
	rec.ConfigJSON = string(configJSON)
	return rec, nil
}

func decodeRepoRegistrationConfig(rec store.RepoRegistrationRecord) (*config.FullConfig, error) {
	var payload repoRegistrationPayload
	if err := json.Unmarshal([]byte(rec.ConfigJSON), &payload); err != nil {
		return nil, fmt.Errorf("decode registration config: %w", err)
	}
	cfg := &config.FullConfig{
		Global: config.GlobalConfig{
			Repo:        rec.Repo,
			Environment: rec.Environment,
		},
		Agents:    make(map[string]*config.AgentConfig, len(payload.Agents)),
		Workflows: make(map[string]*config.WorkflowConfig, len(payload.Workflows)),
	}
	for _, agent := range payload.Agents {
		if agent == nil {
			continue
		}
		agentCopy := *agent
		agentCopy.SourcePath = ""
		if _, err := config.NormalizeAgentConfig(&agentCopy); err != nil {
			return nil, fmt.Errorf("normalize agent %q: %w", agentCopy.Name, err)
		}
		cfg.Agents[agentCopy.Name] = &agentCopy
	}
	for _, workflow := range payload.Workflows {
		if workflow == nil {
			continue
		}
		workflowCopy := *workflow
		cfg.Workflows[workflowCopy.Name] = &workflowCopy
	}
	if err := config.ValidateWorkflowRegistration(cfg.Workflows); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (pm *pollerManager) loadExisting() error {
	regs, err := pm.store.ListRepoRegistrations()
	if err != nil {
		return err
	}
	for _, rec := range regs {
		if rec.Status != "active" {
			continue
		}
		if err := pm.StartOrUpdate(rec); err != nil {
			return err
		}
	}
	return nil
}

func (pm *pollerManager) run() {
	for {
		select {
		case <-pm.rootCtx.Done():
			pm.stopAll()
			return
		case ev := <-pm.events:
			pm.handleEvent(ev)
		}
	}
}

func (pm *pollerManager) StartOrUpdate(rec store.RepoRegistrationRecord) error {
	cfg, err := decodeRepoRegistrationConfig(rec)
	if err != nil {
		return err
	}

	p := poller.NewPoller(pm.ghReader, pm.store, rec.Repo, pm.pollInterval)
	if err := p.PreCheck(); err != nil {
		return err
	}

	if err := pm.stopRepo(rec.Repo); err != nil {
		return err
	}

	dispatchCh := make(chan statemachine.DispatchRequest, dispatchChanSize)
	sm := statemachine.NewStateMachine(cfg.Workflows, pm.store, dispatchCh, pm.eventlog, pm.alertBus)
	depResolver := dependency.NewResolver(pm.store, pm.ghReader, pm.eventlog, pm.alertBus)
	rt := router.NewRouter(cfg.Agents, pm.registry, pm.store, rec.Repo, pm.repoRoot, nil, nil, false)
	runCtx, cancel := context.WithCancel(pm.rootCtx)
	runtime := &repoRuntime{
		registration: rec,
		config:       cfg,
		stateMachine: sm,
		depResolver:  depResolver,
		dispatchCh:   dispatchCh,
		cancel:       cancel,
		done:         make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := rt.Run(runCtx, dispatchCh); err != nil {
			log.Printf("[coordinator] router error for %s: %v", rec.Repo, err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := p.Run(runCtx); err != nil {
			log.Printf("[coordinator] poller error for %s: %v", rec.Repo, err)
		}
	}()
	go func() {
		defer wg.Done()
		for ev := range p.Events() {
			select {
			case pm.events <- ev:
			case <-runCtx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(runtime.done)
		wg.Wait()
	}()

	pm.mu.Lock()
	pm.runtimes[rec.Repo] = runtime
	pm.mu.Unlock()
	return nil
}

func (pm *pollerManager) Deregister(repo string) error {
	if err := pm.stopRepo(repo); err != nil {
		return err
	}
	if err := pm.store.FailPendingTasksForRepo(repo); err != nil {
		return err
	}
	return pm.store.DeleteRepoRegistration(repo)
}

func (pm *pollerManager) ListStatuses() ([]repoStatus, error) {
	regs, err := pm.store.ListRepoRegistrations()
	if err != nil {
		return nil, err
	}
	statuses := make([]repoStatus, 0, len(regs))
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, rec := range regs {
		pollerStatus := "stopped"
		if _, ok := pm.runtimes[rec.Repo]; ok {
			pollerStatus = "running"
		}
		statuses = append(statuses, repoStatus{
			Registration: rec,
			PollerStatus: pollerStatus,
		})
	}
	return statuses, nil
}

func (pm *pollerManager) IsRegistered(repo string) (bool, error) {
	rec, err := pm.store.GetRepoRegistration(repo)
	if err != nil {
		return false, err
	}
	return rec != nil && rec.Status == "active", nil
}

func (pm *pollerManager) MarkAgentCompleted(repo string, issueNum int, taskID, agentName string, exitCode int, currentLabels []string) {
	pm.mu.RLock()
	runtime := pm.runtimes[repo]
	pm.mu.RUnlock()
	if runtime == nil {
		return
	}
	runtime.stateMachine.MarkAgentCompleted(repo, issueNum, taskID, agentName, exitCode, currentLabels)
}

func (pm *pollerManager) handleEvent(ev poller.ChangeEvent) {
	pm.mu.RLock()
	runtime := pm.runtimes[ev.Repo]
	pm.mu.RUnlock()
	if runtime == nil {
		return
	}
	if ev.Type == poller.EventPollCycleDone {
		pm.eventlog.Log(poller.EventPollCycleDone, ev.Repo, 0, map[string]any{"source": "poller"})
		if !runtime.depsResolvedThisCycle {
			pm.runDependencyMaintenance(runtime)
		}
		runtime.depsResolvedThisCycle = false
		runtime.stateMachine.ResetDedup()
		return
	}
	if !runtime.depsResolvedThisCycle {
		pm.runDependencyMaintenance(runtime)
		runtime.depsResolvedThisCycle = true
	}
	if err := runtime.stateMachine.HandleEvent(pm.rootCtx, statemachine.ChangeEvent{
		Type:     ev.Type,
		Repo:     ev.Repo,
		IssueNum: ev.IssueNum,
		Labels:   ev.Labels,
		Detail:   ev.Detail,
	}); err != nil {
		log.Printf("[coordinator] state machine error for %s: %v", ev.Repo, err)
	}
}

func (pm *pollerManager) runDependencyMaintenance(runtime *repoRuntime) {
	runtime.depGraphVersion++
	unblockedIssues, err := runtime.depResolver.EvaluateOpenIssues(pm.rootCtx, runtime.registration.Repo, runtime.depGraphVersion)
	if err != nil {
		log.Printf("[coordinator] dependency resolver error for %s: %v", runtime.registration.Repo, err)
		return
	}
	for _, issueNum := range unblockedIssues {
		if delErr := pm.store.DeleteIssueCache(runtime.registration.Repo, issueNum); delErr != nil {
			log.Printf("[coordinator] dependency unblock cache-invalidate %s#%d: %v", runtime.registration.Repo, issueNum, delErr)
		}
	}
	caches, err := pm.store.ListIssueCaches(runtime.registration.Repo)
	if err != nil {
		log.Printf("[coordinator] dependency reaction list-caches error for %s: %v", runtime.registration.Repo, err)
		return
	}
	for _, cached := range caches {
		if cached.State != "open" {
			continue
		}
		state, err := pm.store.QueryIssueDependencyState(cached.Repo, cached.IssueNum)
		if err != nil || state == nil {
			continue
		}
		wantBlocked := state.Verdict == store.DependencyVerdictBlocked || state.Verdict == store.DependencyVerdictNeedsHuman
		if wantBlocked == state.LastReactionBlocked {
			continue
		}
		if err := pm.reporter.SetBlockedReaction(pm.rootCtx, cached.Repo, cached.IssueNum, wantBlocked); err != nil {
			log.Printf("[coordinator] dependency reaction set %s#%d blocked=%v: %v", cached.Repo, cached.IssueNum, wantBlocked, err)
			continue
		}
		if err := pm.store.MarkDependencyReactionApplied(cached.Repo, cached.IssueNum, wantBlocked); err != nil {
			log.Printf("[coordinator] dependency reaction mark %s#%d: %v", cached.Repo, cached.IssueNum, err)
		}
	}
}

func (pm *pollerManager) stopAll() {
	pm.mu.RLock()
	repos := make([]string, 0, len(pm.runtimes))
	for repo := range pm.runtimes {
		repos = append(repos, repo)
	}
	pm.mu.RUnlock()
	for _, repo := range repos {
		if err := pm.stopRepo(repo); err != nil {
			log.Printf("[coordinator] stop repo %s: %v", repo, err)
		}
	}
}

func (pm *pollerManager) stopRepo(repo string) error {
	pm.mu.Lock()
	runtime := pm.runtimes[repo]
	if runtime != nil {
		delete(pm.runtimes, repo)
	}
	pm.mu.Unlock()
	if runtime == nil {
		return nil
	}
	runtime.cancel()
	select {
	case <-runtime.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out stopping repo runtime %s", repo)
	}
}
