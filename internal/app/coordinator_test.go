package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type noopEventRecorder struct{}

func (noopEventRecorder) Log(string, string, int, interface{}) {}

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
	sm := statemachine.NewStateMachine(
		map[string]*config.WorkflowConfig{"dev-flow": testWorkflowConfig()},
		st,
		dispatchCh,
		noopEventRecorder{},
		nil,
	)
	if err := sm.HandleEvent(t.Context(), statemachine.ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "owner/repo",
		IssueNum: 41,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-dispatchCh:
	case <-t.Context().Done():
		t.Fatal("expected dispatch to create inflight state")
	}

	pm := &PollerManager{
		rootCtx:  t.Context(),
		store:    st,
		runtimes: map[string]*RepoRuntime{"owner/repo": {StateMachine: sm}},
	}
	server := &FullCoordinatorServer{
		Store:       st,
		Pollers:     pm,
		AuthEnabled: true,
		AuthToken:   "shared-secret",
	}
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
		t.Fatalf("second clear status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestHandleRegisterWorkerAuditURLValidation guards the should-fix #2
// behaviour: HandleRegisterWorker must reject audit_url values whose
// scheme is not http/https or whose host is empty (the coordinator
// dials this URL with net/http.Client, so `javascript:`, `file:`,
// `data:` etc. are nonsense at best and confused-deputy bait at worst).
// Empty audit_url stays accepted (means "no audit listener configured").
//
// We assert by wiring HandleRegisterWorker against a server with no
// Pollers/Registry: the audit_url validation block runs before any of
// those, so a 400 with the audit_url-specific error proves the check
// fired. Fallthrough requests reach the WorkerID-required error
// instead, which proves the validation block did NOT trip.
func TestHandleRegisterWorkerAuditURLValidation(t *testing.T) {
	server := &FullCoordinatorServer{}

	cases := []struct {
		name             string
		auditURL         string
		wantStatus       int
		wantBodyContains string
	}{
		{
			name:             "javascript scheme rejected",
			auditURL:         "javascript:alert(1)",
			wantStatus:       http.StatusBadRequest,
			wantBodyContains: "audit_url must be an http or https URL",
		},
		{
			name:             "file scheme rejected",
			auditURL:         "file:///etc/passwd",
			wantStatus:       http.StatusBadRequest,
			wantBodyContains: "audit_url must be an http or https URL",
		},
		{
			name:             "missing host rejected",
			auditURL:         "http://",
			wantStatus:       http.StatusBadRequest,
			wantBodyContains: "audit_url must be an http or https URL",
		},
		{
			name:             "empty audit_url accepted (falls through to worker_id check)",
			auditURL:         "",
			wantStatus:       http.StatusBadRequest,
			wantBodyContains: "worker_id, repo, and roles are required",
		},
		{
			name:             "valid http accepted (falls through to worker_id check)",
			auditURL:         "http://worker:8091",
			wantStatus:       http.StatusBadRequest,
			wantBodyContains: "worker_id, repo, and roles are required",
		},
		{
			name:             "valid https accepted (falls through to worker_id check)",
			auditURL:         "https://worker.example.com:8443",
			wantStatus:       http.StatusBadRequest,
			wantBodyContains: "worker_id, repo, and roles are required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(WorkerRegisterRequest{AuditURL: tc.auditURL})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/register", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			server.HandleRegisterWorker(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(tc.wantBodyContains)) {
				t.Fatalf("body = %s, want substring %q", rec.Body.String(), tc.wantBodyContains)
			}
		})
	}
}

func testWorkflowConfig() *config.WorkflowConfig {
	return &config.WorkflowConfig{
		Name:    "dev-flow",
		Trigger: config.WorkflowTrigger{IssueLabel: "workbuddy"},
		States: map[string]*config.State{
			"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
		},
	}
}
