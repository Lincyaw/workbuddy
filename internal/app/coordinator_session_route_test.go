package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestHandleAnnounceSessionUpsertsRoute exercises the on-creation
// announce path: workers POST a route as soon as they create a session
// so the resolver can find the owning worker before the next register.
func TestHandleAnnounceSessionUpsertsRoute(t *testing.T) {
	st := newCoordinatorTestStore(t)
	if err := st.InsertWorker(store.WorkerRecord{
		ID: "worker-a", Repo: "org/repo", Roles: `["dev"]`, Hostname: "host", Status: "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	server := &FullCoordinatorServer{Store: st}

	body, _ := json.Marshal(SessionAnnouncePayload{
		SessionID: "sess-announced",
		Repo:      "org/repo",
		IssueNum:  42,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/worker-a/sessions/announce", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.HandleAnnounceSession(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got, err := st.GetSessionRoute("sess-announced")
	if err != nil || got == nil {
		t.Fatalf("GetSessionRoute: %v %+v", err, got)
	}
	if got.WorkerID != "worker-a" || got.IssueNum != 42 {
		t.Fatalf("route = %+v", got)
	}
}

// TestHandleAnnounceSessionRejectsUnknownWorker — the announce path
// must not be a free upsert primitive: a session can only be claimed by
// a worker that is currently registered. Otherwise auth-token
// compromise plus a forged route could redirect the proxy at any host.
func TestHandleAnnounceSessionRejectsUnknownWorker(t *testing.T) {
	st := newCoordinatorTestStore(t)
	server := &FullCoordinatorServer{Store: st}

	body, _ := json.Marshal(SessionAnnouncePayload{SessionID: "x", Repo: "org/r", IssueNum: 1})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/ghost/sessions/announce", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.HandleAnnounceSession(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404 for unknown worker", rec.Code, rec.Body.String())
	}
	if got, _ := st.GetSessionRoute("x"); got != nil {
		t.Fatalf("route was inserted for unknown worker: %+v", got)
	}
}

// TestHandleAnnounceSessionRejectsEmptySessionID — defensive: the
// resolver treats an empty string as "no route", so silently letting an
// empty session_id row in would surface no observable bug, just db
// noise. Reject explicitly so misbehaving workers see the error early.
func TestHandleAnnounceSessionRejectsEmptySessionID(t *testing.T) {
	st := newCoordinatorTestStore(t)
	if err := st.InsertWorker(store.WorkerRecord{ID: "worker-a", Repo: "org/r", Roles: `["dev"]`, Hostname: "h", Status: "online"}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	server := &FullCoordinatorServer{Store: st}

	body, _ := json.Marshal(SessionAnnouncePayload{SessionID: "", Repo: "org/r", IssueNum: 1})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/worker-a/sessions/announce", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.HandleAnnounceSession(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestParseAnnounceSessionPath — the prefix mount on
// /api/v1/workers/ catches every worker-scoped URL; the suffix-based
// dispatch must only fire on the exact announce path or a fat-fingered
// /api/v1/workers/foo/sessions/announce/something will accidentally
// route to the announce handler.
func TestParseAnnounceSessionPath(t *testing.T) {
	cases := []struct {
		path    string
		want    string
		wantOK  bool
	}{
		{path: "/api/v1/workers/foo/sessions/announce", want: "foo", wantOK: true},
		{path: "/api/v1/workers/foo/sessions/announce/", want: "", wantOK: false},
		{path: "/api/v1/workers/foo/bar/sessions/announce", want: "", wantOK: false},
		{path: "/api/v1/workers//sessions/announce", want: "", wantOK: false},
		{path: "/api/v1/workers/foo", want: "", wantOK: false},
	}
	for _, tc := range cases {
		got, ok := parseAnnounceSessionPath(tc.path)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseAnnounceSessionPath(%q) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.wantOK)
		}
	}
}
