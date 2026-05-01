package workerclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAnnounceSessionPostsJSON pins the wire contract: AnnounceSession
// hits /api/v1/workers/{id}/sessions/announce with a Bearer token and
// the SessionAnnounce body. Anything off here would ripple into the
// coordinator handler test passing locally but failing in production
// against a stale worker build.
func TestAnnounceSessionPostsJSON(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotMethod string
		gotBody   SessionAnnounce
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "secret-token", &http.Client{Timeout: 2 * time.Second})
	err := c.AnnounceSession(context.Background(), "worker-a", SessionAnnounce{
		SessionID: "sess-1",
		Repo:      "org/repo",
		IssueNum:  42,
	})
	if err != nil {
		t.Fatalf("AnnounceSession: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/api/v1/workers/worker-a/sessions/announce" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody.SessionID != "sess-1" || gotBody.Repo != "org/repo" || gotBody.IssueNum != 42 {
		t.Errorf("body = %+v", gotBody)
	}
}

// TestRegisterCarriesOpenSessions pins the OpenSessions field on
// register: workers re-seed open routes after a coord restart through
// this field, and the coord-side bulk upsert depends on it being
// JSON-serialised.
func TestRegisterCarriesOpenSessions(t *testing.T) {
	var got RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "tok", &http.Client{Timeout: 2 * time.Second})
	if err := c.Register(context.Background(), RegisterRequest{
		WorkerID: "worker-a",
		Repo:     "org/repo",
		Roles:    []string{"dev"},
		OpenSessions: []SessionAnnounce{
			{SessionID: "sess-1", Repo: "org/repo", IssueNum: 1},
			{SessionID: "sess-2", Repo: "org/repo", IssueNum: 2},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(got.OpenSessions) != 2 {
		t.Fatalf("open_sessions len = %d", len(got.OpenSessions))
	}
	if got.OpenSessions[0].SessionID != "sess-1" || got.OpenSessions[1].SessionID != "sess-2" {
		t.Fatalf("open_sessions = %+v", got.OpenSessions)
	}
}
