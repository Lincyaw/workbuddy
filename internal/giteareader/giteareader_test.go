package giteareader

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// routeHTTP is a fake HTTPDoer that returns a canned JSON body keyed by the
// request URL path+query, recording the last request for assertions. No
// network is ever touched.
type routeHTTP struct {
	routes   map[string]string // path (with query) -> JSON body
	lastReq  *http.Request
	failCode int // when non-zero, every response uses this status code
}

func (r *routeHTTP) Do(req *http.Request) (*http.Response, error) {
	r.lastReq = req
	key := req.URL.Path
	if req.URL.RawQuery != "" {
		key += "?" + req.URL.RawQuery
	}
	body, ok := r.routes[key]
	code := http.StatusOK
	if r.failCode != 0 {
		code = r.failCode
	} else if !ok {
		code = http.StatusNotFound
		body = `{"message":"no route: ` + key + `"}`
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func TestListIssues_ParsesLabelsAndExcludesPRs(t *testing.T) {
	hc := &routeHTTP{routes: map[string]string{
		"/api/v1/repos/org/repo/issues?type=issues&state=open&limit=100": `[
			{"number":7,"title":"Add feature","body":"do the thing","state":"open",
			 "user":{"login":"alice"},
			 "labels":[{"name":"role:dev"},{"name":"status:developing"}]},
			{"number":8,"title":"A PR row","body":"","state":"open",
			 "user":{"login":"bob"},"labels":[],
			 "pull_request":{"merged":false}}
		]`,
	}}
	r := New(hc, "https://gitea.example.com/", "tok")

	issues, err := r.ListIssues("org/repo")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue (PR excluded), got %d: %+v", len(issues), issues)
	}
	got := issues[0]
	if got.Number != 7 || got.Title != "Add feature" || got.Body != "do the thing" {
		t.Fatalf("issue fields mismatch: %+v", got)
	}
	if got.State != "open" || got.Author != "alice" {
		t.Fatalf("state/author mismatch: %+v", got)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "role:dev" || got.Labels[1] != "status:developing" {
		t.Fatalf("labels mismatch: %v", got.Labels)
	}
	// Auth header + URL assembly.
	if h := hc.lastReq.Header.Get("Authorization"); h != "token tok" {
		t.Fatalf("auth header mismatch: %q", h)
	}
	if hc.lastReq.URL.Host != "gitea.example.com" {
		t.Fatalf("host mismatch: %q", hc.lastReq.URL.Host)
	}
}

func TestListPRs_MapsHeadRefAndURL(t *testing.T) {
	hc := &routeHTTP{routes: map[string]string{
		"/api/v1/repos/org/repo/pulls?state=open&limit=100": `[
			{"number":42,"html_url":"https://gitea.example.com/org/repo/pulls/42",
			 "state":"open","head":{"ref":"workbuddy/issue-7"}}
		]`,
	}}
	r := New(hc, "https://gitea.example.com", "tok")

	prs, err := r.ListPRs("org/repo")
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(prs))
	}
	pr := prs[0]
	if pr.Number != 42 || pr.State != "open" {
		t.Fatalf("number/state mismatch: %+v", pr)
	}
	if pr.URL != "https://gitea.example.com/org/repo/pulls/42" {
		t.Fatalf("url mismatch: %q", pr.URL)
	}
	if pr.Branch != "workbuddy/issue-7" {
		t.Fatalf("branch (head ref) mismatch: %q", pr.Branch)
	}
}

func TestReadIssue_MapsDetailFields(t *testing.T) {
	hc := &routeHTTP{routes: map[string]string{
		"/api/v1/repos/org/repo/issues/7": `{
			"number":7,"title":"t","body":"the body","state":"Closed",
			"labels":[{"name":"status:done"}]}`,
	}}
	r := New(hc, "https://gitea.example.com", "tok")

	det, err := r.ReadIssue("org/repo", 7)
	if err != nil {
		t.Fatalf("ReadIssue: %v", err)
	}
	if det.Number != 7 || det.Body != "the body" {
		t.Fatalf("number/body mismatch: %+v", det)
	}
	if det.State != "closed" {
		t.Fatalf("state should be lower-cased, got %q", det.State)
	}
	if len(det.Labels) != 1 || det.Labels[0] != "status:done" {
		t.Fatalf("labels mismatch: %v", det.Labels)
	}
	// Documented gaps: Gitea has no stateReason / closedByLinkedPR equivalent.
	if det.StateReason != "" {
		t.Fatalf("StateReason should be empty for gitea, got %q", det.StateReason)
	}
	if det.ClosedByLinkedPR {
		t.Fatalf("ClosedByLinkedPR should be false for gitea")
	}
}

func TestReadIssueComments_ParsesAuthorBodyCreatedAt(t *testing.T) {
	hc := &routeHTTP{routes: map[string]string{
		"/api/v1/repos/org/repo/issues/7/comments": `[
			{"user":{"login":"carol"},"body":"first","created_at":"2026-06-06T10:00:00Z"},
			{"user":{"login":"dan"},"body":"second","created_at":"2026-06-06T11:30:00Z"}
		]`,
	}}
	r := New(hc, "https://gitea.example.com", "tok")

	comments, err := r.ReadIssueComments("org/repo", 7)
	if err != nil {
		t.Fatalf("ReadIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	if comments[0].Author != "carol" || comments[0].Body != "first" {
		t.Fatalf("comment[0] mismatch: %+v", comments[0])
	}
	if comments[0].CreatedAt != "2026-06-06T10:00:00Z" {
		t.Fatalf("comment[0] createdAt mismatch (want RFC3339): %q", comments[0].CreatedAt)
	}
	if comments[1].Author != "dan" {
		t.Fatalf("comment[1] author mismatch: %+v", comments[1])
	}
}

func TestCheckRepoAccess_OKAndError(t *testing.T) {
	ok := &routeHTTP{routes: map[string]string{
		"/api/v1/repos/org/repo": `{"name":"repo"}`,
	}}
	if err := New(ok, "https://gitea.example.com", "tok").CheckRepoAccess("org/repo"); err != nil {
		t.Fatalf("expected access OK, got %v", err)
	}

	denied := &routeHTTP{failCode: http.StatusForbidden}
	err := New(denied, "https://gitea.example.com", "tok").CheckRepoAccess("org/repo")
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("expected 403 error, got %v", err)
	}
}

func TestGet_NonJSONOnErrorStatusSurfacesBody(t *testing.T) {
	hc := &routeHTTP{failCode: http.StatusInternalServerError, routes: map[string]string{
		"/api/v1/repos/org/repo/issues?type=issues&state=open&limit=100": `boom`,
	}}
	_, err := New(hc, "https://gitea.example.com", "tok").ListIssues("org/repo")
	if err == nil || !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected status 500 with body, got %v", err)
	}
}
