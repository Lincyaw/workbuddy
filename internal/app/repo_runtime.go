package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/security"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// DispatchChanSize is the buffer size for the state-machine → router
// dispatch channel. Kept package-local because the channel never crosses
// the repo-runtime boundary.
const DispatchChanSize = 64

// RepoRegistrationPayload is the serialized view of a repo registration as
// stored in SQLite (ConfigJSON) and as posted over HTTP.
type RepoRegistrationPayload struct {
	Repo         string                   `json:"repo"`
	Environment  string                   `json:"environment,omitempty"`
	PollInterval time.Duration            `json:"poll_interval,omitempty"`
	Agents       []*config.AgentConfig    `json:"agents"`
	Workflows    []*config.WorkflowConfig `json:"workflows"`
}

// RepoRuntime is the per-repo runtime: state machine, dependency resolver,
// dispatch channel, and a cancel handle for graceful shutdown.
type RepoRuntime struct {
	Registration          store.RepoRegistrationRecord
	Config                *config.FullConfig
	StateMachine          *statemachine.StateMachine
	DepResolver           *dependency.Resolver
	DispatchCh            chan statemachine.DispatchRequest
	cancel                context.CancelFunc
	done                  chan struct{}
	depsResolvedThisCycle bool
	depGraphVersion       int64
}

// RepoStatus is the read model returned by PollerManager.ListStatuses.
type RepoStatus struct {
	Registration store.RepoRegistrationRecord
	PollerStatus string
}

// PollerManager owns the multi-repo runtime. It spawns per-repo goroutines
// (poller + state machine + router) on StartOrUpdate, fans events into a
// single channel, and shuts runtimes down on Deregister or parent cancel.
type PollerManager struct {
	rootCtx      context.Context
	store        *store.Store
	registry     *registry.Registry
	eventlog     *eventlog.EventLogger
	alertBus     *alertbus.Bus
	ghReader     poller.GHReader
	reporter     *reporter.Reporter
	repoRoot     string
	pollInterval time.Duration
	security     *security.Runtime

	mu       sync.RWMutex
	runtimes map[string]*RepoRuntime
	events   chan poller.ChangeEvent
}

// NewPollerManager wires a PollerManager and starts its event-dispatch
// goroutine. The returned manager is ready to accept StartOrUpdate calls.
func NewPollerManager(ctx context.Context, st *store.Store, reg *registry.Registry, evlog *eventlog.EventLogger, ab *alertbus.Bus, ghReader poller.GHReader, rep *reporter.Reporter, repoRoot string, pollInterval time.Duration, secRuntime *security.Runtime) *PollerManager {
	pm := &PollerManager{
		rootCtx:      ctx,
		store:        st,
		registry:     reg,
		eventlog:     evlog,
		alertBus:     ab,
		ghReader:     ghReader,
		reporter:     rep,
		repoRoot:     repoRoot,
		pollInterval: pollInterval,
		security:     secRuntime,
		runtimes:     make(map[string]*RepoRuntime),
		events:       make(chan poller.ChangeEvent, 256),
	}
	go pm.run()
	return pm
}

// BuildRepoRegistrationPayload extracts the wire-level registration payload
// from a loaded config, stripping source paths so the same payload can be
// serialized to SQLite or shipped to the coordinator via HTTP.
func BuildRepoRegistrationPayload(cfg *config.FullConfig) *RepoRegistrationPayload {
	payload := &RepoRegistrationPayload{
		Repo:         strings.TrimSpace(cfg.Global.Repo),
		Environment:  strings.TrimSpace(cfg.Global.Environment),
		PollInterval: cfg.Global.PollInterval,
		Agents:       make([]*config.AgentConfig, 0, len(cfg.Agents)),
		Workflows:    make([]*config.WorkflowConfig, 0, len(cfg.Workflows)),
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

// BuildRepoRegistrationRecord validates the payload, normalizes it through
// a round-trip decode/encode, and returns a RepoRegistrationRecord ready to
// be upserted into the store.
func BuildRepoRegistrationRecord(payload *RepoRegistrationPayload) (store.RepoRegistrationRecord, error) {
	if payload == nil {
		return store.RepoRegistrationRecord{}, fmt.Errorf("repo registration payload is required")
	}

	candidate := &RepoRegistrationPayload{
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
	cfg, err := DecodeRepoRegistrationConfig(rec)
	if err != nil {
		return store.RepoRegistrationRecord{}, err
	}

	normalizedPayload := BuildRepoRegistrationPayload(cfg)
	configJSON, err = json.Marshal(normalizedPayload)
	if err != nil {
		return store.RepoRegistrationRecord{}, fmt.Errorf("marshal normalized registration config: %w", err)
	}
	rec.Environment = normalizedPayload.Environment
	rec.ConfigJSON = string(configJSON)
	return rec, nil
}

// DecodeRepoRegistrationConfig parses a registration record back into a
// FullConfig, running the same agent/workflow normalization as the config
// loader.
func DecodeRepoRegistrationConfig(rec store.RepoRegistrationRecord) (*config.FullConfig, error) {
	var payload RepoRegistrationPayload
	if err := json.Unmarshal([]byte(rec.ConfigJSON), &payload); err != nil {
		return nil, fmt.Errorf("decode registration config: %w", err)
	}
	cfg := &config.FullConfig{
		Global: config.GlobalConfig{
			Repo:         rec.Repo,
			Environment:  rec.Environment,
			PollInterval: payload.PollInterval,
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

// LoadExisting enumerates active registrations from the store and spawns a
// runtime for each. Called at coordinator startup.
func (pm *PollerManager) LoadExisting() error {
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

func (pm *PollerManager) run() {
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

// StartOrUpdate stops any existing runtime for rec.Repo and starts a fresh
// one using the config in rec.ConfigJSON. Safe to call repeatedly on config
// reloads.
func (pm *PollerManager) StartOrUpdate(rec store.RepoRegistrationRecord) error {
	cfg, err := DecodeRepoRegistrationConfig(rec)
	if err != nil {
		return err
	}

	interval := cfg.Global.PollInterval
	if interval <= 0 {
		interval = pm.pollInterval
	}
	p := poller.NewPoller(pm.ghReader, pm.store, rec.Repo, interval)
	if err := p.PreCheck(); err != nil {
		return err
	}

	if err := pm.stopRepo(rec.Repo); err != nil {
		return err
	}

	dispatchCh := make(chan statemachine.DispatchRequest, DispatchChanSize)
	sm := statemachine.NewStateMachine(cfg.Workflows, pm.store, dispatchCh, pm.eventlog, pm.alertBus)
	// Enable the per-issue dispatch claim (REQ-057) so concurrent coordinators
	// on different machines cannot race each other through the same SQLite DB.
	// The claimer id is process-scoped so two coordinators on the same host do
	// not collapse onto the same logical owner.
	sm.SetIssueClaim(BuildIssueClaimerID("coordinator-"+HostnameOrUnknown(), os.Getpid()), statemachine.DefaultIssueClaimLease)
	if pm.reporter != nil {
		pm.reporter.SetCycleCapTrailLoader(NewCycleCapTrailLoader(pm.store))
		sm.SetCycleCapReporter(&cycleCapReporterAdapter{rep: pm.reporter})
	}
	depResolver := dependency.NewResolver(pm.store, pm.ghReader, pm.eventlog, pm.alertBus)
	rt := router.NewRouter(cfg.Agents, pm.registry, pm.store, rec.Repo, pm.repoRoot, nil, nil, false)
	rt.SetWorkflows(cfg.Workflows)
	if issueDataReader, ok := pm.ghReader.(router.IssueDataReader); ok {
		rt.SetIssueDataReader(issueDataReader)
	}
	runCtx, cancel := context.WithCancel(pm.rootCtx)
	runtime := &RepoRuntime{
		Registration: rec,
		Config:       cfg,
		StateMachine: sm,
		DepResolver:  depResolver,
		DispatchCh:   dispatchCh,
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
	pm.recoverOrphanedActiveStates(runtime)
	return nil
}

func (pm *PollerManager) recoverOrphanedActiveStates(runtime *RepoRuntime) {
	if runtime == nil || runtime.Config == nil {
		return
	}

	caches, err := pm.store.ListIssueCaches(runtime.Registration.Repo)
	if err != nil {
		log.Printf("[coordinator] recovery: list issue cache for %s: %v", runtime.Registration.Repo, err)
		return
	}

	for _, cached := range caches {
		if cached.State != "open" {
			continue
		}

		labels, err := parseCachedLabels(cached.Labels)
		if err != nil {
			log.Printf("[coordinator] recovery: decode labels for %s#%d: %v", cached.Repo, cached.IssueNum, err)
			continue
		}

		workflowName, stateName, agents, ok, err := recoverableActiveState(runtime.Config, labels)
		if err != nil {
			log.Printf("[coordinator] recovery: skip %s#%d: %v", cached.Repo, cached.IssueNum, err)
			continue
		}
		if !ok {
			continue
		}

		active, err := pm.store.HasAnyActiveTask(cached.Repo, cached.IssueNum)
		if err != nil {
			log.Printf("[coordinator] recovery: query active task for %s#%d: %v", cached.Repo, cached.IssueNum, err)
			continue
		}
		if active {
			continue
		}

		log.Printf(
			"[coordinator] recovery: re-dispatching orphaned active state for %s#%d workflow=%s state=%s agents=%v",
			cached.Repo, cached.IssueNum, workflowName, stateName, agents,
		)
		if err := runtime.StateMachine.HandleEvent(pm.rootCtx, statemachine.ChangeEvent{
			Type:     poller.EventIssueCreated,
			Repo:     cached.Repo,
			IssueNum: cached.IssueNum,
			Labels:   labels,
		}); err != nil {
			log.Printf("[coordinator] recovery: failed to re-dispatch %s#%d: %v", cached.Repo, cached.IssueNum, err)
		}
	}
}

func parseCachedLabels(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil, fmt.Errorf("parse cached labels: %w", err)
	}
	return labels, nil
}

func recoverableActiveState(cfg *config.FullConfig, labels []string) (workflowName, stateName string, agents []string, ok bool, err error) {
	labelSet := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		labelSet[label] = struct{}{}
	}

	for _, wf := range cfg.Workflows {
		triggerLabel := strings.TrimSpace(wf.Trigger.IssueLabel)
		if triggerLabel == "" {
			continue
		}
		if _, exists := labelSet[triggerLabel]; !exists {
			continue
		}

		for name, state := range wf.States {
			if state.EnterLabel == "" {
				continue
			}
			if _, exists := labelSet[state.EnterLabel]; !exists {
				continue
			}
			stateAgents := recoverableStateAgents(state)
			if len(stateAgents) == 0 {
				continue
			}
			if ok {
				return "", "", nil, false, fmt.Errorf("multiple active workflow states match labels %v", labels)
			}
			workflowName = wf.Name
			stateName = name
			agents = stateAgents
			ok = true
		}
	}

	return workflowName, stateName, agents, ok, nil
}

func recoverableStateAgents(state *config.State) []string {
	if len(state.Agents) > 0 {
		return append([]string(nil), state.Agents...)
	}
	if strings.TrimSpace(state.Agent) == "" {
		return nil
	}
	return []string{state.Agent}
}

// Deregister stops the runtime for repo, fails any pending tasks, and
// removes the registration row.
func (pm *PollerManager) Deregister(repo string) error {
	if err := pm.stopRepo(repo); err != nil {
		return err
	}
	if err := pm.store.FailPendingTasksForRepo(repo); err != nil {
		return err
	}
	return pm.store.DeleteRepoRegistration(repo)
}

// ListStatuses returns the current status of every registered repo,
// combining the persisted record with whether a runtime goroutine is live.
func (pm *PollerManager) ListStatuses() ([]RepoStatus, error) {
	regs, err := pm.store.ListRepoRegistrations()
	if err != nil {
		return nil, err
	}
	statuses := make([]RepoStatus, 0, len(regs))
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, rec := range regs {
		pollerStatus := "stopped"
		if _, ok := pm.runtimes[rec.Repo]; ok {
			pollerStatus = "running"
		}
		statuses = append(statuses, RepoStatus{
			Registration: rec,
			PollerStatus: pollerStatus,
		})
	}
	return statuses, nil
}

// IsRegistered returns true when the given repo has an active registration
// in the store.
func (pm *PollerManager) IsRegistered(repo string) (bool, error) {
	rec, err := pm.store.GetRepoRegistration(repo)
	if err != nil {
		return false, err
	}
	return rec != nil && rec.Status == "active", nil
}

// MarkAgentCompleted forwards an agent completion signal to the state
// machine of the target repo, when one is running.
func (pm *PollerManager) MarkAgentCompleted(repo string, issueNum int, taskID, agentName string, exitCode int, currentLabels []string) {
	pm.MarkAgentCompletedWithDecision(repo, issueNum, taskID, agentName, exitCode, currentLabels, nil)
}

func (pm *PollerManager) MarkAgentCompletedWithDecision(repo string, issueNum int, taskID, agentName string, exitCode int, currentLabels []string, decision *runtimepkg.SynthesisDecision) {
	pm.mu.RLock()
	runtime := pm.runtimes[repo]
	pm.mu.RUnlock()
	if runtime == nil {
		return
	}
	runtime.StateMachine.MarkAgentCompletedWithDecision(repo, issueNum, taskID, agentName, exitCode, currentLabels, decision)
}

// ClearInflight removes one repo runtime's in-memory inflight entry.
func (pm *PollerManager) ClearInflight(repo string, issueNum int) (*statemachine.InflightGroupSnapshot, bool) {
	pm.mu.RLock()
	runtime := pm.runtimes[repo]
	pm.mu.RUnlock()
	if runtime == nil {
		return nil, false
	}
	return runtime.StateMachine.ClearInflight(repo, issueNum)
}

func (pm *PollerManager) handleEvent(ev poller.ChangeEvent) {
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
		runtime.StateMachine.ResetDedup()
		return
	}
	if !AllowSecurityEvent(pm.security, ev) {
		return
	}
	if !runtime.depsResolvedThisCycle {
		pm.runDependencyMaintenance(runtime)
		runtime.depsResolvedThisCycle = true
	}
	if err := runtime.StateMachine.HandleEvent(pm.rootCtx, statemachine.ChangeEvent{
		Type:     ev.Type,
		Repo:     ev.Repo,
		IssueNum: ev.IssueNum,
		Labels:   ev.Labels,
		Detail:   ev.Detail,
		Author:   ev.Author,
	}); err != nil {
		log.Printf("[coordinator] state machine error for %s: %v", ev.Repo, err)
	}
}

func (pm *PollerManager) runDependencyMaintenance(runtime *RepoRuntime) {
	runtime.depGraphVersion++
	unblockedIssues, err := runtime.DepResolver.EvaluateOpenIssues(pm.rootCtx, runtime.Registration.Repo, runtime.depGraphVersion)
	if err != nil {
		log.Printf("[coordinator] dependency resolver error for %s: %v", runtime.Registration.Repo, err)
		return
	}
	for _, issueNum := range unblockedIssues {
		if delErr := pm.store.DeleteIssueCache(runtime.Registration.Repo, issueNum); delErr != nil {
			log.Printf("[coordinator] dependency unblock cache-invalidate %s#%d: %v", runtime.Registration.Repo, issueNum, delErr)
		}
	}
	caches, err := pm.store.ListIssueCaches(runtime.Registration.Repo)
	if err != nil {
		log.Printf("[coordinator] dependency reaction list-caches error for %s: %v", runtime.Registration.Repo, err)
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

func (pm *PollerManager) stopAll() {
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

// Shutdown synchronously stops every running repo runtime. It is safe to call
// multiple times and is used by coordinator shutdown to ensure claim-release
// cleanup finishes before the store closes.
func (pm *PollerManager) Shutdown() {
	pm.stopAll()
}

func (pm *PollerManager) stopRepo(repo string) error {
	pm.mu.Lock()
	runtime := pm.runtimes[repo]
	if runtime != nil {
		delete(pm.runtimes, repo)
	}
	pm.mu.Unlock()
	if runtime == nil {
		return nil
	}
	runtime.StateMachine.ReleaseAllIssueClaims()
	runtime.cancel()
	select {
	case <-runtime.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out stopping repo runtime %s", repo)
	}
}
