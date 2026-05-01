package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/router"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/Lincyaw/workbuddy/internal/taskprep"
)

// FullCoordinatorServer is the distributed coordinator's HTTP control plane:
// repo/worker registration, task claim (long-poll), task result/heartbeat/
// release, and config reload. It delegates poll+dispatch to PollerManager
// and live config reload to CoordinatorConfigRuntime.
type FullCoordinatorServer struct {
	RootCtx     context.Context
	Store       *store.Store
	Registry    *registry.Registry
	Eventlog    *eventlog.EventLogger
	TaskHub     *tasknotify.Hub
	Pollers     *PollerManager
	Config      *CoordinatorConfigRuntime
	AuthEnabled bool
	AuthToken   string
	// CookieInsecure drops the Secure attribute on session cookies. Default
	// false (Secure required); set true only for HTTP reverse-proxy fronts
	// where TLS terminates upstream.
	CookieInsecure bool
}

// SessionCookieName is the HTTP cookie that carries the bearer token after
// browser login. Cookie value is the bearer token verbatim (no server-side
// session table; trade-off accepted in #215 design).
const SessionCookieName = "wb_session"

// SessionCookieMaxAge is the lifetime of the wb_session cookie (8 hours).
const SessionCookieMaxAge = 8 * 60 * 60

// WorkerRegisterRequest is the body of POST /api/v1/workers/register.
type WorkerRegisterRequest struct {
	WorkerID    string   `json:"worker_id"`
	Repo        string   `json:"repo"`
	Roles       []string `json:"roles"`
	Runtime     string   `json:"runtime,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	Hostname    string   `json:"hostname"`
	MgmtBaseURL string   `json:"mgmt_base_url,omitempty"`
}

// TaskPollResponse is returned from GET /api/v1/tasks/poll when a task is
// claimable.
type TaskPollResponse struct {
	TaskID         string                       `json:"task_id"`
	Repo           string                       `json:"repo"`
	IssueNum       int                          `json:"issue_num"`
	AgentName      string                       `json:"agent_name"`
	Workflow       string                       `json:"workflow,omitempty"`
	State          string                       `json:"state,omitempty"`
	RolloutIndex   int                          `json:"rollout_index,omitempty"`
	RolloutsTotal  int                          `json:"rollouts_total,omitempty"`
	RolloutGroupID string                       `json:"rollout_group_id,omitempty"`
	Roles          []string                     `json:"roles,omitempty"`
	Synthesis      *runtimepkg.SynthesisContext `json:"synthesis,omitempty"`
}

// RepoRegisterRequest is the body of POST /api/v1/repos/register.
type RepoRegisterRequest struct {
	Repo        string                   `json:"repo"`
	Environment string                   `json:"environment,omitempty"`
	Agents      []*config.AgentConfig    `json:"agents"`
	Workflows   []*config.WorkflowConfig `json:"workflows"`
}

// RepoStatusResponse is an element of GET /api/v1/repos.
type RepoStatusResponse struct {
	Repo         string    `json:"repo"`
	Environment  string    `json:"environment"`
	Status       string    `json:"status"`
	PollerStatus string    `json:"poller_status"`
	RegisteredAt time.Time `json:"registered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TaskResultRequest is the body of POST /api/v1/tasks/{id}/result.
type TaskResultRequest struct {
	WorkerID      string   `json:"worker_id"`
	Status        string   `json:"status"`
	CurrentLabels []string `json:"current_labels"`
	// InfraFailure flags launcher-layer failures that must NOT be translated
	// into a state-machine failure signal. See issue #131 / AC-3.
	InfraFailure      bool                          `json:"infra_failure,omitempty"`
	InfraReason       string                        `json:"infra_reason,omitempty"`
	SynthesisDecision *runtimepkg.SynthesisDecision `json:"synthesis_decision,omitempty"`
}

// TaskHeartbeatRequest is the body of POST /api/v1/tasks/{id}/heartbeat.
type TaskHeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
}

// TaskReleaseRequest is the body of POST /api/v1/tasks/{id}/release.
type TaskReleaseRequest struct {
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason,omitempty"`
}

// HandleClearIssueInflight serves
// POST /api/v1/admin/issues/{owner}/{repo}/{issue_num}/clear-inflight.
func (s *FullCoordinatorServer) HandleClearIssueInflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	repo, issueNum, ok := parseClearIssueInflightPath(r.URL.Path)
	if !ok {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid issue path; expect /api/v1/admin/issues/<owner>/<repo>/<num>/clear-inflight"})
		return
	}
	if s.Pollers == nil {
		CoordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "poller manager unavailable"})
		return
	}
	snapshot, cleared := s.Pollers.ClearInflight(repo, issueNum)
	if !cleared || snapshot == nil {
		CoordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "no inflight entry for issue"})
		return
	}
	CoordWriteJSON(w, http.StatusOK, map[string]any{
		"repo":      repo,
		"issue_num": issueNum,
		"group":     snapshot,
	})
}

func parseClearIssueInflightPath(path string) (string, int, bool) {
	trimmed := strings.TrimPrefix(path, "/api/v1/admin/issues/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 4 || parts[len(parts)-1] != "clear-inflight" {
		return "", 0, false
	}
	issueNum, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || issueNum <= 0 {
		return "", 0, false
	}
	repo := strings.Join(parts[:len(parts)-2], "/")
	if strings.TrimSpace(repo) == "" {
		return "", 0, false
	}
	return repo, issueNum, true
}

// HandleConfigReload serves POST /api/v1/config/reload.
func (s *FullCoordinatorServer) HandleConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.Config == nil {
		CoordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "config reload is unavailable without --config-dir bootstrap mode"})
		return
	}
	summary, err := s.Config.Reload("manual_api")
	if err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	CoordWriteJSON(w, http.StatusOK, summary)
}

// WrapAuth returns a handler that enforces Bearer-token auth when enabled.
// It accepts either an `Authorization: Bearer <token>` header (API clients)
// or a `wb_session` cookie carrying the same token (browser sessions). When
// auth fails on a request that prefers HTML, it 302s to /login?next=<path>;
// otherwise it returns 401 JSON to keep API clients machine-friendly.
func (s *FullCoordinatorServer) WrapAuth(next http.Handler) http.Handler {
	if !s.AuthEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requestAuthorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		if prefersHTML(r) {
			redirectToLogin(w, r)
			return
		}
		CoordWriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	})
}

// requestAuthorized returns true when the request carries a valid bearer
// token in either the Authorization header or the wb_session cookie.
func (s *FullCoordinatorServer) requestAuthorized(r *http.Request) bool {
	const prefix = "Bearer "
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, prefix) {
		token := strings.TrimSpace(strings.TrimPrefix(authz, prefix))
		if s.isAuthorizedBearer(token) {
			return true
		}
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if s.isAuthorizedBearer(strings.TrimSpace(c.Value)) {
			return true
		}
	}
	return false
}

// prefersHTML reports whether the client's Accept header signals a browser
// (i.e. wants HTML). Empty/missing Accept defaults to API semantics.
func prefersHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// redirectToLogin issues a 302 to /login?next=<original path+query>.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	target := "/login"
	if next != "" && next != "/login" {
		target = "/login?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *FullCoordinatorServer) isAuthorizedBearer(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	if s.AuthToken != "" && token == s.AuthToken {
		return true
	}
	if s.Store == nil {
		return false
	}
	_, err := s.Store.AuthenticateWorkerToken(token)
	return err == nil
}

// HandleHealth serves GET /health.
func (s *FullCoordinatorServer) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	statuses, err := s.Pollers.ListStatuses()
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	CoordWriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"repos":  len(statuses),
	})
}

// HandleRegisterRepo serves POST /api/v1/repos/register.
func (s *FullCoordinatorServer) HandleRegisterRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req RepoRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "repo is required"})
		return
	}

	payload := RepoRegistrationPayload{
		Repo:        req.Repo,
		Environment: strings.TrimSpace(req.Environment),
		Agents:      req.Agents,
		Workflows:   req.Workflows,
	}
	rec, err := BuildRepoRegistrationRecord(&payload)
	if err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	prev, err := s.Store.GetRepoRegistration(req.Repo)
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Store.UpsertRepoRegistration(rec); err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Pollers.StartOrUpdate(rec); err != nil {
		if prev != nil {
			if restoreErr := s.Store.UpsertRepoRegistration(*prev); restoreErr != nil {
				CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v (rollback failed: %v)", err, restoreErr)})
				return
			}
		} else {
			if deleteErr := s.Store.DeleteRepoRegistration(req.Repo); deleteErr != nil {
				CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v (rollback failed: %v)", err, deleteErr)})
				return
			}
		}
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	CoordWriteJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

// HandleListRepos serves GET /api/v1/repos.
func (s *FullCoordinatorServer) HandleListRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	statuses, err := s.Pollers.ListStatuses()
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := make([]RepoStatusResponse, 0, len(statuses))
	for _, status := range statuses {
		resp = append(resp, RepoStatusResponse{
			Repo:         status.Registration.Repo,
			Environment:  status.Registration.Environment,
			Status:       status.Registration.Status,
			PollerStatus: status.PollerStatus,
			RegisteredAt: status.Registration.RegisteredAt,
			UpdatedAt:    status.Registration.UpdatedAt,
		})
	}
	CoordWriteJSON(w, http.StatusOK, resp)
}

// HandleRepoByPath serves DELETE /api/v1/repos/{repo}.
func (s *FullCoordinatorServer) HandleRepoByPath(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	repo = strings.TrimSpace(repo)
	if repo == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.Pollers.Deregister(repo); err != nil {
			CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		CoordWriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// HandleWorkerByPath serves DELETE /api/v1/workers/{worker_id}.
func (s *FullCoordinatorServer) HandleWorkerByPath(w http.ResponseWriter, r *http.Request) {
	workerID := strings.TrimPrefix(r.URL.Path, "/api/v1/workers/")
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.Registry.Unregister(workerID); err != nil {
			switch {
			case errors.Is(err, registry.ErrWorkerNotFound):
				CoordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			case errors.Is(err, registry.ErrWorkerHasRunningTask):
				CoordWriteJSON(w, http.StatusConflict, map[string]string{"error": "worker has a running task"})
			default:
				CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}
		CoordWriteJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
	default:
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// HandleRegisterWorker serves POST /api/v1/workers/register.
func (s *FullCoordinatorServer) HandleRegisterWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req WorkerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	req.Repo = strings.TrimSpace(req.Repo)
	req.Runtime = strings.TrimSpace(req.Runtime)
	req.MgmtBaseURL = strings.TrimRight(strings.TrimSpace(req.MgmtBaseURL), "/")
	if len(req.Repos) == 0 && req.Repo != "" {
		req.Repos = []string{req.Repo}
	}
	if req.Repo == "" && len(req.Repos) > 0 {
		req.Repo = strings.TrimSpace(req.Repos[0])
	}
	if strings.TrimSpace(req.Runtime) == "" {
		req.Runtime = config.RuntimeClaudeCode
	}
	if req.WorkerID == "" || req.Repo == "" || len(req.Roles) == 0 {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id, repo, and roles are required"})
		return
	}
	for _, repo := range req.Repos {
		registered, err := s.Pollers.IsRegistered(repo)
		if err != nil {
			CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !registered {
			CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("repo %q is not registered", repo)})
			return
		}
	}
	if err := s.Registry.RegisterWithRepos(req.WorkerID, req.Repo, req.Repos, req.Roles, req.Runtime, req.Hostname, req.MgmtBaseURL); err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.Eventlog.Log(eventlog.TypeWorkerRegistered, req.Repo, 0, map[string]any{
		"worker_id": req.WorkerID,
		"roles":     req.Roles,
		"runtime":   req.Runtime,
		"repos":     req.Repos,
		"hostname":  req.Hostname,
		"mgmt_url":  req.MgmtBaseURL,
	})
	CoordWriteJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

// HandlePollTask serves GET /api/v1/tasks/poll with long-polling.
func (s *FullCoordinatorServer) HandlePollTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
	if workerID == "" {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	timeout := ParseLongPollTimeout(r.URL.Query().Get("timeout"))
	worker, err := s.lookupWorker(workerID)
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if worker == nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown worker"})
		return
	}
	// Every poll counts as a liveness signal — bump heartbeat up front so an
	// idle worker (no task to claim) doesn't get flagged as missing.
	_ = s.Registry.Heartbeat(worker.ID)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(LongPollCheckInterval)
	defer ticker.Stop()

	for {
		task, err := s.claimNextTask(worker)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrTaskClaimConflict):
		default:
			CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if task != nil {
			_ = s.Registry.Heartbeat(worker.ID)
			CoordWriteJSON(w, http.StatusOK, task)
			return
		}

		select {
		case <-s.RootCtx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-ticker.C:
		}
	}
}

// HandleTaskAction dispatches POST /api/v1/tasks/{id}/{result|heartbeat|release}.
func (s *FullCoordinatorServer) HandleTaskAction(w http.ResponseWriter, r *http.Request) {
	taskID, action, ok := parseFullTaskActionPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && action == "result":
		s.HandleTaskResult(w, r, taskID)
	case r.Method == http.MethodPost && action == "heartbeat":
		s.HandleTaskHeartbeat(w, r, taskID)
	case r.Method == http.MethodPost && action == "release":
		s.HandleTaskRelease(w, r, taskID)
	default:
		CoordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// HandleTaskResult processes a terminal task status submission from a worker.
func (s *FullCoordinatorServer) HandleTaskResult(w http.ResponseWriter, r *http.Request, taskID string) {
	var req TaskResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	task, err := s.Store.GetTask(taskID)
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		CoordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if req.WorkerID == "" || task.WorkerID != req.WorkerID {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id does not match claimed task"})
		return
	}
	status := NormalizeTaskResultStatus(req.Status)
	if status == "" {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be completed, failed, or timeout"})
		return
	}
	if err := s.Store.TransitionTaskStatusIfRunning(taskID, status); err != nil {
		if errors.Is(err, store.ErrTaskStatusTerminal) {
			// Late submit from a zombie goroutine after the task was already
			// settled (by another goroutine of the same worker, by operator
			// cleanup, etc.). Reject without rewriting the terminal status.
			// See #143 / #141 for the dup-claim race that produces these.
			log.Printf("[coordinator] rejecting late submit for task %s: %v", taskID, err)
			CoordWriteJSON(w, http.StatusConflict, map[string]string{"error": "task already in terminal status"})
			return
		}
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Registry.Heartbeat(req.WorkerID); err != nil {
		log.Printf("[coordinator] worker heartbeat during result failed: %v", err)
	}
	exitCode := 0
	if status != store.TaskStatusCompleted {
		exitCode = 1
	}
	PublishTaskCompletion(s.TaskHub, router.WorkerTask{
		TaskID:    task.ID,
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
	}, status, exitCode, time.Now(), time.Now())
	if req.InfraFailure {
		// Launcher-layer failure: the agent never got to decide. Record the
		// infra event for operator visibility, emit the standard completed
		// event for bookkeeping, but DO NOT call MarkAgentCompleted — that
		// would tell the state-machine the agent FAILED, which is the very
		// mis-classification issue #131 is fixing.
		s.Eventlog.Log(eventlog.TypeInfraFailure, task.Repo, task.IssueNum, map[string]any{
			"task_id":    task.ID,
			"worker_id":  req.WorkerID,
			"agent_name": task.AgentName,
			"status":     status,
			"reason":     req.InfraReason,
			"source":     "worker_submit",
		})
	} else {
		s.Pollers.MarkAgentCompletedWithDecision(task.Repo, task.IssueNum, task.ID, task.AgentName, exitCode, req.CurrentLabels, req.SynthesisDecision)
	}
	s.Eventlog.Log(eventlog.TypeCompleted, task.Repo, task.IssueNum, map[string]any{
		"task_id":    task.ID,
		"worker_id":  req.WorkerID,
		"agent_name": task.AgentName,
		"status":     status,
	})
	CoordWriteJSON(w, http.StatusOK, map[string]string{"status": status})
}

// HandleTaskHeartbeat extends a running task's claim lease.
func (s *FullCoordinatorServer) HandleTaskHeartbeat(w http.ResponseWriter, r *http.Request, taskID string) {
	var req TaskHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if req.WorkerID == "" {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	task, err := s.Store.GetTask(taskID)
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		CoordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if task.WorkerID != req.WorkerID {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id does not match claimed task"})
		return
	}
	if err := s.Store.HeartbeatTask(taskID, req.WorkerID, DefaultLongPollTimeout); err != nil {
		log.Printf("[coordinator] task heartbeat DB update failed for %s: %v", taskID, err)
	}
	if err := s.Registry.Heartbeat(req.WorkerID); err != nil {
		if errors.Is(err, registry.ErrWorkerNotFound) {
			CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown worker"})
			return
		}
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleTaskRelease returns a claimed task to the pool without marking it
// terminal.
func (s *FullCoordinatorServer) HandleTaskRelease(w http.ResponseWriter, r *http.Request, taskID string) {
	var req TaskReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if req.WorkerID == "" {
		CoordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	released, err := s.Store.ReleaseTask(taskID, req.WorkerID)
	if err != nil {
		CoordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !released {
		CoordWriteJSON(w, http.StatusConflict, map[string]string{"error": "task is not claimable by this worker"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *FullCoordinatorServer) lookupWorker(workerID string) (*store.WorkerRecord, error) {
	return s.Store.GetWorker(workerID)
}

func (s *FullCoordinatorServer) claimNextTask(worker *store.WorkerRecord) (*TaskPollResponse, error) {
	var roles []string
	if err := json.Unmarshal([]byte(worker.Roles), &roles); err != nil {
		return nil, fmt.Errorf("unmarshal worker roles: %w", err)
	}
	var repos []string
	if err := json.Unmarshal([]byte(worker.ReposJSON), &repos); err != nil || len(repos) == 0 {
		repos = []string{worker.Repo}
	}
	task, err := s.Store.ClaimNextTask(worker.ID, roles, repos, worker.Runtime, "", DefaultLongPollTimeout)
	if err != nil || task == nil {
		return nil, err
	}
	s.Eventlog.Log(eventlog.TypeDispatch, task.Repo, task.IssueNum, map[string]any{
		"task_id":    task.ID,
		"worker_id":  worker.ID,
		"agent_name": task.AgentName,
	})
	return &TaskPollResponse{
		TaskID:         task.ID,
		Repo:           task.Repo,
		IssueNum:       task.IssueNum,
		AgentName:      task.AgentName,
		Workflow:       task.Workflow,
		State:          task.State,
		RolloutIndex:   task.RolloutIndex,
		RolloutsTotal:  task.RolloutsTotal,
		RolloutGroupID: task.RolloutGroupID,
		Roles:          append([]string(nil), roles...),
		Synthesis:      s.buildSynthesisPayload(task),
	}, nil
}

func (s *FullCoordinatorServer) buildSynthesisPayload(task *store.TaskRecord) *runtimepkg.SynthesisContext {
	if task == nil || s.Config == nil {
		return nil
	}
	cfg := s.Config.Current()
	if cfg == nil {
		return nil
	}
	wf, ok := cfg.Workflows[task.Workflow]
	if !ok || wf == nil {
		return nil
	}
	state, ok := wf.States[task.State]
	if !ok || state == nil || state.Mode != config.StateModeSynth {
		return nil
	}
	sourceState := ""
	for name, candidate := range wf.States {
		if candidate == nil {
			continue
		}
		for _, target := range candidate.Transitions {
			if target == task.State {
				sourceState = name
				break
			}
		}
		if sourceState != "" {
			break
		}
	}
	gh := ghadapter.NewCLI()
	relatedPRs, err := gh.ListRelatedPRs(task.Repo, task.IssueNum)
	if err != nil {
		return nil
	}
	synth, err := taskprep.BuildSynthesisContext(task.Repo, task.IssueNum, task.Workflow, sourceState, state, s.Store, gh, relatedPRs)
	if err != nil {
		return nil
	}
	return synth
}

// ParseLongPollTimeout parses a human-readable timeout, falling back to the
// default when unset or malformed.
func ParseLongPollTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultLongPollTimeout
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return DefaultLongPollTimeout
}

func parseFullTaskActionPath(path string) (taskID string, action string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/api/v1/tasks/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// NormalizeTaskResultStatus normalizes a raw status string to one of the
// persisted TaskStatus* constants, returning "" when the status is unknown.
func NormalizeTaskResultStatus(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case store.TaskStatusCompleted:
		return store.TaskStatusCompleted
	case store.TaskStatusFailed:
		return store.TaskStatusFailed
	case store.TaskStatusTimeout:
		return store.TaskStatusTimeout
	default:
		return ""
	}
}

// CoordWriteJSON is the canonical JSON response helper for the coordinator
// HTTP surface.
func CoordWriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[coordinator] encode response failed: %v", err)
	}
}
