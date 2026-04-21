package app

import (
	"context"
	"fmt"
	"sync"
)

// RunningTasks is a thread-safe registry of cancel functions for running
// agent tasks, keyed by repo#issueNum. The embedded worker registers a
// cancel when dispatch begins so the serve loop can abort it when the issue
// is closed on GitHub mid-run.
type RunningTasks struct {
	mu    sync.Mutex
	tasks map[string]context.CancelFunc
}

// NewRunningTasks creates a new RunningTasks registry.
func NewRunningTasks() *RunningTasks {
	return &RunningTasks{tasks: make(map[string]context.CancelFunc)}
}

func runningTaskKey(repo string, issue int) string {
	return fmt.Sprintf("%s#%d", repo, issue)
}

// Register stores a cancel function for a running task.
func (rt *RunningTasks) Register(repo string, issue int, cancel context.CancelFunc) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tasks[runningTaskKey(repo, issue)] = cancel
}

// Cancel cancels the running task for the given repo+issue and returns true,
// or returns false if no such task is running.
func (rt *RunningTasks) Cancel(repo string, issue int) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	key := runningTaskKey(repo, issue)
	cancel, ok := rt.tasks[key]
	if !ok {
		return false
	}
	cancel()
	delete(rt.tasks, key)
	return true
}

// Remove removes the entry for a completed task (without cancelling).
func (rt *RunningTasks) Remove(repo string, issue int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.tasks, runningTaskKey(repo, issue))
}

// ClosedIssues tracks issues that were closed while same-issue work was still
// queued so deferred tasks can be dropped before they start.
type ClosedIssues struct {
	issues sync.Map
}

func (c *ClosedIssues) MarkClosed(repo string, issue int) {
	c.issues.Store(runningTaskKey(repo, issue), struct{}{})
}

func (c *ClosedIssues) MarkOpen(repo string, issue int) {
	c.issues.Delete(runningTaskKey(repo, issue))
}

func (c *ClosedIssues) IsClosed(repo string, issue int) bool {
	_, ok := c.issues.Load(runningTaskKey(repo, issue))
	return ok
}
