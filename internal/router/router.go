// Package router dispatches agent tasks to available workers.
package router

import (
	"context"
	"fmt"
	"log"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/google/uuid"
)

// WorkerTask is the unit of work sent to an embedded Worker via channel.
type WorkerTask struct {
	TaskID    string
	Repo      string
	IssueNum  int
	AgentName string
	Agent     *config.AgentConfig
	Context   *launcher.TaskContext
	Workflow  string
	State     string
}

// Router receives DispatchRequests from the StateMachine and routes them
// to Workers. In v0.1.0 it sends tasks over a Go channel to the embedded Worker.
type Router struct {
	agents   map[string]*config.AgentConfig
	registry *registry.Registry
	store    *store.Store
	repo     string
	taskChan chan<- WorkerTask
}

// NewRouter creates a Router for v0.1.0 channel-based dispatch.
func NewRouter(
	agents map[string]*config.AgentConfig,
	reg *registry.Registry,
	st *store.Store,
	repo string,
	taskChan chan<- WorkerTask,
) *Router {
	return &Router{
		agents:   agents,
		registry: reg,
		store:    st,
		repo:     repo,
		taskChan: taskChan,
	}
}

// Run reads from the dispatch channel and routes tasks. Blocks until ctx is cancelled.
func (r *Router) Run(ctx context.Context, dispatchCh <-chan statemachine.DispatchRequest) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case req, ok := <-dispatchCh:
			if !ok {
				return nil
			}
			r.handleDispatch(ctx, req)
		}
	}
}

// handleDispatch processes a single DispatchRequest.
func (r *Router) handleDispatch(ctx context.Context, req statemachine.DispatchRequest) {
	agent, ok := r.agents[req.AgentName]
	if !ok {
		log.Printf("[router] agent %q not found, skipping dispatch for %s#%d", req.AgentName, req.Repo, req.IssueNum)
		return
	}

	taskID := uuid.New().String()

	// Record task in store
	if err := r.store.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      req.Repo,
		IssueNum:  req.IssueNum,
		AgentName: req.AgentName,
		Status:    "pending",
	}); err != nil {
		log.Printf("[router] failed to insert task: %v", err)
		return
	}

	taskCtx := &launcher.TaskContext{
		Issue: launcher.IssueContext{
			Number: req.IssueNum,
		},
		Repo: req.Repo,
		Session: launcher.SessionContext{
			ID: fmt.Sprintf("session-%s", taskID),
		},
	}

	task := WorkerTask{
		TaskID:    taskID,
		Repo:      req.Repo,
		IssueNum:  req.IssueNum,
		AgentName: req.AgentName,
		Agent:     agent,
		Context:   taskCtx,
		Workflow:  req.Workflow,
		State:     req.State,
	}

	select {
	case r.taskChan <- task:
		if err := r.store.UpdateTaskStatus(taskID, "running"); err != nil {
			log.Printf("[router] failed to update task status: %v", err)
		}
	case <-ctx.Done():
		return
	}
}
