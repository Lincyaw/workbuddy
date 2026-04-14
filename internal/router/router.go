package router

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/google/uuid"
)

// GHReader abstracts GitHub read operations needed by the router.
type GHReader interface {
	ViewIssue(repo string, issueNum int) (*IssueDetail, error)
}

// IssueDetail holds the full issue info returned by gh issue view --json.
type IssueDetail struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
	State  string   `json:"state"`
}

// TaskMessage is sent to the embedded Worker via Go channel in v0.1.0.
type TaskMessage struct {
	TaskID  string
	Agent   *config.AgentConfig
	Context *launcher.TaskContext
}

// EventRecorder abstracts event logging so tests can use a fake.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// Router receives DispatchRequests from the State Machine, resolves agent
// definitions and matching workers, records tasks in SQLite, and sends
// work to the embedded Worker via a Go channel (v0.1.0).
type Router struct {
	agents   map[string]*config.AgentConfig
	registry *registry.Registry
	store    *store.Store
	gh       GHReader
	eventlog EventRecorder
	taskCh   chan<- TaskMessage

	mu sync.Mutex
}

// NewRouter creates a Router.
func NewRouter(
	agents map[string]*config.AgentConfig,
	reg *registry.Registry,
	st *store.Store,
	gh GHReader,
	el EventRecorder,
	taskCh chan<- TaskMessage,
) *Router {
	return &Router{
		agents:   agents,
		registry: reg,
		store:    st,
		gh:       gh,
		eventlog: el,
		taskCh:   taskCh,
	}
}

// Run consumes DispatchRequests from the dispatch channel until it is closed.
func (r *Router) Run(dispatch <-chan statemachine.DispatchRequest) {
	for req := range dispatch {
		if err := r.handleDispatch(req); err != nil {
			log.Printf("[router] error handling dispatch: %v", err)
		}
	}
}

// handleDispatch processes a single DispatchRequest.
func (r *Router) handleDispatch(req statemachine.DispatchRequest) error {
	// 1. Look up agent definition.
	agent, ok := r.agents[req.AgentName]
	if !ok {
		r.eventlog.Log("error", req.Repo, req.IssueNum,
			fmt.Sprintf(`{"error":"agent definition not found","agent":"%s"}`, req.AgentName))
		log.Printf("[router] agent %q not found, skipping dispatch for %s#%d", req.AgentName, req.Repo, req.IssueNum)
		return nil // skip, don't crash
	}

	// 2. Find matching Worker via Registry (repo + role).
	workers, err := r.registry.FindWorkers(req.Repo, agent.Role)
	if err != nil {
		return fmt.Errorf("router: find workers: %w", err)
	}

	// Generate task ID.
	taskID := uuid.New().String()

	if len(workers) == 0 {
		// No matching worker -- task stays pending in SQLite.
		if err := r.store.InsertTask(store.TaskRecord{
			ID:        taskID,
			Repo:      req.Repo,
			IssueNum:  req.IssueNum,
			AgentName: req.AgentName,
			WorkerID:  "",
			Status:    "pending",
		}); err != nil {
			return fmt.Errorf("router: insert pending task: %w", err)
		}
		r.eventlog.Log("task_pending", req.Repo, req.IssueNum,
			fmt.Sprintf(`{"task_id":"%s","agent":"%s","reason":"no matching worker"}`, taskID, req.AgentName))
		log.Printf("[router] no matching worker for agent %q (repo=%s, role=%s), task %s pending",
			req.AgentName, req.Repo, agent.Role, taskID)
		return nil
	}

	// Pick the first matching worker (simple strategy for v0.1.0).
	worker := workers[0]

	// 3. Build task context: call gh issue view --json.
	issueDetail, err := r.gh.ViewIssue(req.Repo, req.IssueNum)
	if err != nil {
		log.Printf("[router] warning: failed to fetch issue detail for %s#%d: %v", req.Repo, req.IssueNum, err)
		// Continue with empty detail rather than failing the dispatch.
		issueDetail = &IssueDetail{Number: req.IssueNum}
	}

	sessionID := uuid.New().String()
	taskCtx := &launcher.TaskContext{
		Issue: launcher.IssueContext{
			Number: issueDetail.Number,
			Title:  issueDetail.Title,
			Body:   issueDetail.Body,
			Labels: issueDetail.Labels,
		},
		Repo: req.Repo,
		Session: launcher.SessionContext{
			ID: sessionID,
		},
	}

	// 4. Record task in SQLite task_queue.
	if err := r.store.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      req.Repo,
		IssueNum:  req.IssueNum,
		AgentName: req.AgentName,
		WorkerID:  worker.ID,
		Status:    "dispatched",
	}); err != nil {
		return fmt.Errorf("router: insert task: %w", err)
	}

	// 5. Send task to embedded Worker via Go channel.
	r.taskCh <- TaskMessage{
		TaskID:  taskID,
		Agent:   agent,
		Context: taskCtx,
	}

	payloadJSON, _ := json.Marshal(map[string]string{
		"task_id":   taskID,
		"agent":     req.AgentName,
		"worker_id": worker.ID,
		"session":   sessionID,
	})
	r.eventlog.Log("dispatch", req.Repo, req.IssueNum, string(payloadJSON))

	log.Printf("[router] dispatched task %s: agent=%s worker=%s issue=%s#%d",
		taskID, req.AgentName, worker.ID, req.Repo, req.IssueNum)

	return nil
}
