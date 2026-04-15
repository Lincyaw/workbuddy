package http

import (
	"bytes"
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestServer(st *store.Store) *httptest.Server {
	mux := nethttp.NewServeMux()
	NewHandler(st).Register(mux)
	return httptest.NewServer(mux)
}

func postJSON(t *testing.T, client *nethttp.Client, url string, body any) *nethttp.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := nethttp.NewRequest(nethttp.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func decodeTaskResponse(t *testing.T, resp *nethttp.Response) taskResponse {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var got taskResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return got
}

func TestClaimAckHeartbeatCompleteFlow(t *testing.T) {
	st := newTestStore(t)
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-1",
		Repo:      "Lincyaw/workbuddy",
		IssueNum:  41,
		AgentName: "dev-agent",
		Role:      "dev",
		Runtime:   "codex-exec",
		Workflow:  "default",
		State:     "developing",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	srv := newTestServer(st)
	defer srv.Close()

	client := srv.Client()
	claimResp := postJSON(t, client, srv.URL+"/v1/tasks/claim", claimRequest{
		WorkerID:       "worker-a",
		Roles:          []string{"dev"},
		IdempotencyKey: "claim-1",
		LongPollSecs:   1,
		LeaseSecs:      60,
	})
	if claimResp.StatusCode != nethttp.StatusOK {
		t.Fatalf("claim status = %d, want 200", claimResp.StatusCode)
	}
	claimed := decodeTaskResponse(t, claimResp)
	if claimed.ID != "task-1" || claimed.Role != "dev" || claimed.Runtime != "codex-exec" {
		t.Fatalf("unexpected claim payload: %+v", claimed)
	}

	dupResp := postJSON(t, client, srv.URL+"/v1/tasks/claim", claimRequest{
		WorkerID:       "worker-a",
		Roles:          []string{"dev"},
		IdempotencyKey: "claim-1",
		LongPollSecs:   1,
		LeaseSecs:      60,
	})
	if dupResp.StatusCode != nethttp.StatusOK {
		t.Fatalf("duplicate claim status = %d, want 200", dupResp.StatusCode)
	}
	dupClaim := decodeTaskResponse(t, dupResp)
	if dupClaim.ID != claimed.ID {
		t.Fatalf("duplicate claim returned %q, want %q", dupClaim.ID, claimed.ID)
	}

	otherResp := postJSON(t, client, srv.URL+"/v1/tasks/claim", claimRequest{
		WorkerID:     "worker-b",
		Roles:        []string{"dev"},
		LongPollSecs: 1,
		LeaseSecs:    60,
	})
	if otherResp.StatusCode != nethttp.StatusNoContent {
		t.Fatalf("second worker claim status = %d, want 204", otherResp.StatusCode)
	}
	_ = otherResp.Body.Close()

	ackResp := postJSON(t, client, srv.URL+"/v1/tasks/task-1/ack", workerRequest{
		WorkerID:  "worker-a",
		LeaseSecs: 60,
	})
	if ackResp.StatusCode != nethttp.StatusOK {
		t.Fatalf("ack status = %d, want 200", ackResp.StatusCode)
	}
	acked := decodeTaskResponse(t, ackResp)
	if acked.Status != store.TaskStatusRunning {
		t.Fatalf("acked status = %q, want %q", acked.Status, store.TaskStatusRunning)
	}

	heartbeatResp := postJSON(t, client, srv.URL+"/v1/tasks/task-1/heartbeat", workerRequest{
		WorkerID:  "worker-a",
		LeaseSecs: 60,
	})
	if heartbeatResp.StatusCode != nethttp.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200", heartbeatResp.StatusCode)
	}
	_ = heartbeatResp.Body.Close()

	completeResp := postJSON(t, client, srv.URL+"/v1/tasks/task-1/complete", workerRequest{
		WorkerID:    "worker-a",
		ExitCode:    0,
		SessionRefs: []string{"session-123", "/sessions/session-123"},
	})
	if completeResp.StatusCode != nethttp.StatusOK {
		t.Fatalf("complete status = %d, want 200", completeResp.StatusCode)
	}
	completed := decodeTaskResponse(t, completeResp)
	if completed.Status != store.TaskStatusCompleted {
		t.Fatalf("completed status = %q, want %q", completed.Status, store.TaskStatusCompleted)
	}
	if len(completed.SessionRefs) != 2 {
		t.Fatalf("session refs = %v, want 2 refs", completed.SessionRefs)
	}
}

func TestDuplicateClaimRejectedByOwnership(t *testing.T) {
	st := newTestStore(t)
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-2",
		Repo:      "Lincyaw/workbuddy",
		IssueNum:  42,
		AgentName: "review-agent",
		Role:      "review",
		Runtime:   "codex-exec",
		Workflow:  "default",
		State:     "reviewing",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	srv := newTestServer(st)
	defer srv.Close()

	client := srv.Client()
	resp := postJSON(t, client, srv.URL+"/v1/tasks/claim", claimRequest{
		WorkerID:       "worker-a",
		Roles:          []string{"review"},
		IdempotencyKey: "claim-review",
		LongPollSecs:   1,
	})
	if resp.StatusCode != nethttp.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	conflict := postJSON(t, client, srv.URL+"/v1/tasks/task-2/ack", workerRequest{
		WorkerID: "worker-b",
	})
	if conflict.StatusCode != nethttp.StatusConflict {
		t.Fatalf("wrong-worker ack status = %d, want 409", conflict.StatusCode)
	}
	_ = conflict.Body.Close()
}
