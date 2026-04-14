package router

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// --- Fakes ---

type fakeGHReader struct {
	issues map[string]*IssueDetail // key: "repo#num"
	err    error
}

func (f *fakeGHReader) ViewIssue(repo string, issueNum int) (*IssueDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	key := fmt.Sprintf("%s#%d", repo, issueNum)
	if d, ok := f.issues[key]; ok {
		return d, nil
	}
	return &IssueDetail{Number: issueNum}, nil
}

type fakeEventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	Type     string
	Repo     string
	IssueNum int
	Payload  interface{}
}

func (f *fakeEventRecorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{
		Type: eventType, Repo: repo, IssueNum: issueNum, Payload: payload,
	})
}

func (f *fakeEventRecorder) findEvent(eventType string) *recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.events {
		if f.events[i].Type == eventType {
			return &f.events[i]
		}
	}
	return nil
}

// --- Helpers ---

func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func setupTestRegistry(t *testing.T, st *store.Store) *registry.Registry {
	t.Helper()
	return registry.NewRegistry(st, 30*time.Second)
}

// --- Tests ---

func TestRouter_MatchingWorkerFound(t *testing.T) {
	st := setupTestStore(t)
	reg := setupTestRegistry(t, st)

	// Register a worker with matching repo and role.
	if err := reg.Register("worker-1", "owner/repo", []string{"dev"}, "host1"); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "claude-code",
			Command: "echo hello",
		},
	}

	gh := &fakeGHReader{
		issues: map[string]*IssueDetail{
			"owner/repo#42": {
				Number: 42,
				Title:  "Test Issue",
				Body:   "Fix the bug",
				Labels: []string{"status:dev"},
			},
		},
	}
	el := &fakeEventRecorder{}
	taskCh := make(chan TaskMessage, 10)

	r := NewRouter(agents, reg, st, gh, el, taskCh)

	// Simulate dispatch.
	dispatch := make(chan statemachine.DispatchRequest, 1)
	dispatch <- statemachine.DispatchRequest{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		Workflow:  "test-wf",
		State:     "dev",
	}
	close(dispatch)

	r.Run(dispatch)

	// Verify task was sent to channel.
	select {
	case msg := <-taskCh:
		if msg.Agent.Name != "dev-agent" {
			t.Errorf("expected agent dev-agent, got %s", msg.Agent.Name)
		}
		if msg.Context.Issue.Title != "Test Issue" {
			t.Errorf("expected issue title 'Test Issue', got %q", msg.Context.Issue.Title)
		}
		if msg.Context.Repo != "owner/repo" {
			t.Errorf("expected repo owner/repo, got %s", msg.Context.Repo)
		}
		if msg.TaskID == "" {
			t.Error("expected non-empty task ID")
		}
	default:
		t.Fatal("expected task message on channel, got none")
	}

	// Verify task recorded in SQLite.
	tasks, err := st.QueryTasks("")
	if err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != "dispatched" {
		t.Errorf("expected status dispatched, got %s", tasks[0].Status)
	}
	if tasks[0].WorkerID != "worker-1" {
		t.Errorf("expected worker_id worker-1, got %s", tasks[0].WorkerID)
	}

	// Verify dispatch event was logged.
	ev := el.findEvent("dispatch")
	if ev == nil {
		t.Error("expected 'dispatch' event to be logged")
	}
}

func TestRouter_NoMatchingWorker(t *testing.T) {
	st := setupTestStore(t)
	reg := setupTestRegistry(t, st)

	// Register a worker with a DIFFERENT role.
	if err := reg.Register("worker-1", "owner/repo", []string{"test"}, "host1"); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "claude-code",
			Command: "echo hello",
		},
	}

	gh := &fakeGHReader{}
	el := &fakeEventRecorder{}
	taskCh := make(chan TaskMessage, 10)

	r := NewRouter(agents, reg, st, gh, el, taskCh)

	dispatch := make(chan statemachine.DispatchRequest, 1)
	dispatch <- statemachine.DispatchRequest{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
	}
	close(dispatch)

	r.Run(dispatch)

	// Verify NO task was sent to channel.
	select {
	case msg := <-taskCh:
		t.Fatalf("expected no task message, got %+v", msg)
	default:
		// good
	}

	// Verify task is recorded as pending in SQLite.
	tasks, err := st.QueryTasks("pending")
	if err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 pending task, got %d", len(tasks))
	}
	if tasks[0].WorkerID != "" {
		t.Errorf("expected empty worker_id for pending task, got %s", tasks[0].WorkerID)
	}

	ev := el.findEvent("task_pending")
	if ev == nil {
		t.Error("expected 'task_pending' event to be logged")
	}
}

func TestRouter_AgentNotFound(t *testing.T) {
	st := setupTestStore(t)
	reg := setupTestRegistry(t, st)

	// No agents configured.
	agents := map[string]*config.AgentConfig{}

	gh := &fakeGHReader{}
	el := &fakeEventRecorder{}
	taskCh := make(chan TaskMessage, 10)

	r := NewRouter(agents, reg, st, gh, el, taskCh)

	dispatch := make(chan statemachine.DispatchRequest, 1)
	dispatch <- statemachine.DispatchRequest{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "nonexistent-agent",
	}
	close(dispatch)

	r.Run(dispatch)

	// Verify NO task was sent to channel.
	select {
	case msg := <-taskCh:
		t.Fatalf("expected no task message, got %+v", msg)
	default:
		// good
	}

	// Verify no task in SQLite.
	tasks, err := st.QueryTasks("")
	if err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}

	// Verify error event was logged.
	ev := el.findEvent("error")
	if ev == nil {
		t.Error("expected 'error' event to be logged for missing agent")
	}
}

func TestRouter_GHReaderFailure(t *testing.T) {
	// When gh issue view fails, the router should still dispatch with empty detail.
	st := setupTestStore(t)
	reg := setupTestRegistry(t, st)

	if err := reg.Register("worker-1", "owner/repo", []string{"dev"}, "host1"); err != nil {
		t.Fatalf("register worker: %v", err)
	}

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "claude-code",
			Command: "echo hello",
		},
	}

	gh := &fakeGHReader{err: fmt.Errorf("gh: command failed")}
	el := &fakeEventRecorder{}
	taskCh := make(chan TaskMessage, 10)

	r := NewRouter(agents, reg, st, gh, el, taskCh)

	dispatch := make(chan statemachine.DispatchRequest, 1)
	dispatch <- statemachine.DispatchRequest{
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
	}
	close(dispatch)

	r.Run(dispatch)

	// Should still dispatch even if GH read failed.
	select {
	case msg := <-taskCh:
		if msg.Context.Issue.Number != 42 {
			t.Errorf("expected issue number 42, got %d", msg.Context.Issue.Number)
		}
	default:
		t.Fatal("expected task message on channel even with GH failure")
	}
}

// Ensure the test binary can find its temp dir.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
