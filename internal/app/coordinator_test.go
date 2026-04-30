package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func newCoordinatorTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestWrapAuthAcceptsSharedAndWorkerTokens(t *testing.T) {
	st := newCoordinatorTestStore(t)
	issued, err := st.IssueWorkerToken("worker-1", "owner/repo", []string{"dev"}, "host1")
	if err != nil {
		t.Fatalf("IssueWorkerToken: %v", err)
	}

	server := &FullCoordinatorServer{
		Store:       st,
		AuthEnabled: true,
		AuthToken:   "shared-secret",
	}
	protected := server.WrapAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "shared token", token: "shared-secret", wantStatus: http.StatusNoContent},
		{name: "worker token", token: issued.Token, wantStatus: http.StatusNoContent},
		{name: "unknown token", token: "kid.unknown", wantStatus: http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/poll", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()

			protected.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestWrapAuthRejectsRevokedAndUnregisteredWorkerTokens(t *testing.T) {
	st := newCoordinatorTestStore(t)
	revoked, err := st.IssueWorkerToken("worker-revoked", "owner/repo", []string{"dev"}, "host1")
	if err != nil {
		t.Fatalf("IssueWorkerToken revoked: %v", err)
	}
	if err := st.RevokeWorkerToken("worker-revoked", revoked.KID); err != nil {
		t.Fatalf("RevokeWorkerToken: %v", err)
	}

	unregistered, err := st.IssueWorkerToken("worker-deleted", "owner/repo", []string{"dev"}, "host2")
	if err != nil {
		t.Fatalf("IssueWorkerToken unregistered: %v", err)
	}
	deleted, err := st.DeleteWorker("worker-deleted")
	if err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if !deleted {
		t.Fatal("expected issued worker to be deleted")
	}

	server := &FullCoordinatorServer{
		Store:       st,
		AuthEnabled: true,
		AuthToken:   "shared-secret",
	}
	protected := server.WrapAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name  string
		token string
	}{
		{name: "revoked token", token: revoked.Token},
		{name: "unregistered token", token: unregistered.Token},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/poll", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()

			protected.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestHandleClearIssueInflight(t *testing.T) {
	st := newCoordinatorTestStore(t)
	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	sm := statemachine.NewStateMachine(map[string]*config.WorkflowConfig{
		"default": {
			Name:    "default",
			Trigger: config.WorkflowTrigger{IssueLabel: "workbuddy"},
			States: map[string]*config.State{
				"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
			},
		},
	}, st, dispatchCh, eventlog.NewEventLogger(st), nil)
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-41",
		Repo:      "owner/repo",
		IssueNum:  41,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if err := sm.HandleEvent(context.Background(), statemachine.ChangeEvent{
		Type:     "issue_created",
		Repo:     "owner/repo",
		IssueNum: 41,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	<-dispatchCh

	pm := &PollerManager{runtimes: map[string]*RepoRuntime{
		"owner/repo": {StateMachine: sm},
	}}
	server := &FullCoordinatorServer{Pollers: pm, AuthEnabled: true, AuthToken: "shared-secret"}
	protected := server.WrapAuth(http.HandlerFunc(server.HandleClearIssueInflight))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/issues/owner/repo/41/clear-inflight", nil)
	req.Header.Set("Authorization", "Bearer shared-secret")
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if sm.IsInflight("owner/repo", 41) {
		t.Fatal("inflight entry should be cleared")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/issues/owner/repo/41/clear-inflight", nil)
	req.Header.Set("Authorization", "Bearer shared-secret")
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}
