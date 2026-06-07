// Package giteareader is the Gitea implementation of the issue/PR read side
// of the provider abstraction. It satisfies poller.GHReader against the Gitea
// REST API, mirroring the GitHub gh-CLI reader in internal/ghadapter so a
// Gitea-hosted repo can be polled by the same coordinator runtime.
//
// Selection is by repo registration host_kind (see labelwriter.ResolveHostKind):
// PollerManager builds this reader for host_kind=gitea registrations and keeps
// the gh-CLI reader for github/absent. Auth is a GITEA_TOKEN bearer, modelled
// on internal/labelwriter's existing Gitea write path. The HTTP client is
// indirected (HTTPDoer) so tests intercept calls without touching a real Gitea.
//
// See docs/decisions/2026-06-06-runtime-strategy-and-convergence.md §6
// ("Gitea read gap").
package giteareader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/poller"
)

// HTTPDoer is the minimal slice of *http.Client the reader needs. Tests swap
// in a fake to assert the exact wire calls without network access.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// listLimit caps the page size requested from Gitea. It matches the gh-CLI
// reader's --limit 100 so close-detection truncation behaves identically
// across providers (see poller.ghListLimit).
const listLimit = 100

// Reader reads issues and pull requests from a Gitea instance over the REST
// API. It is the Gitea counterpart of ghadapter.CLI and produces the same
// poller.Issue / poller.PR / poller.IssueDetails structs.
type Reader struct {
	http    HTTPDoer
	baseURL string
	token   string
}

// New constructs a Reader for a Gitea instance.
//
//   - baseURL is the instance root (e.g. https://gitea.example.com); a trailing
//     slash is tolerated.
//   - token is the GITEA_TOKEN value used as a bearer; an empty token is
//     allowed here (anonymous reads on public repos) but most instances will
//     reject it and the error surfaces on the first call.
//
// When client is nil, http.DefaultClient is used.
func New(client HTTPDoer, baseURL, token string) *Reader {
	if client == nil {
		client = http.DefaultClient
	}
	return &Reader{
		http:    client,
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
	}
}

// get issues a GET against the Gitea API path (already URL-encoded by the
// caller) and decodes a JSON body into out. Non-2xx responses become errors
// that include the response body for diagnosis.
func (r *Reader) get(ctx context.Context, path string, out any) error {
	if r.baseURL == "" {
		return fmt.Errorf("giteareader: base URL is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	url := r.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("giteareader: build request %s: %w", url, err)
	}
	if r.token != "" {
		req.Header.Set("Authorization", "token "+r.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("giteareader: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("giteareader: read body %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("giteareader: GET %s: status %d body=%s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("giteareader: parse %s: %w", url, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Gitea JSON shapes (only the fields the poller depends on)
// ---------------------------------------------------------------------------

type giteaLabel struct {
	Name string `json:"name"`
}

type giteaUser struct {
	Login string `json:"login"`
}

// giteaIssue maps the Gitea API Issue object. The same endpoint returns both
// issues and PRs; the PullRequest field is non-nil for PRs so we can exclude
// them from the issue list (the gh-CLI reader's `gh issue list` never returns
// PRs).
type giteaIssue struct {
	Number      int          `json:"number"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	State       string       `json:"state"`
	Labels      []giteaLabel `json:"labels"`
	User        giteaUser    `json:"user"`
	PullRequest *struct {
		Merged bool `json:"merged"`
	} `json:"pull_request"`
}

type giteaPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

type giteaComment struct {
	User      giteaUser `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ---------------------------------------------------------------------------
// poller.GHReader implementation
// ---------------------------------------------------------------------------

// ListIssues lists open issues (excluding pull requests, which Gitea returns
// from the same endpoint) and maps them into poller.Issue. The label slice
// carries names only — the whole state machine keys off label names.
func (r *Reader) ListIssues(repo string) ([]poller.Issue, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/issues?type=issues&state=open&limit=%d", repo, listLimit)
	var raw []giteaIssue
	if err := r.get(context.Background(), path, &raw); err != nil {
		return nil, fmt.Errorf("giteareader: list issues for %s: %w", repo, err)
	}
	issues := make([]poller.Issue, 0, len(raw))
	for _, it := range raw {
		// Defensive: even with type=issues, never treat a PR row as an issue.
		if it.PullRequest != nil {
			continue
		}
		issues = append(issues, poller.Issue{
			Number: it.Number,
			Title:  it.Title,
			State:  it.State,
			Labels: labelNames(it.Labels),
			Body:   it.Body,
			Author: it.User.Login,
		})
	}
	return issues, nil
}

// ListPRs lists open pull requests and maps them into poller.PR. Gitea's PR
// URL field is html_url, mapped onto poller.PR.URL; the head ref maps onto
// Branch (the poller derives the linked issue number from this branch name).
func (r *Reader) ListPRs(repo string) ([]poller.PR, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/pulls?state=open&limit=%d", repo, listLimit)
	var raw []giteaPR
	if err := r.get(context.Background(), path, &raw); err != nil {
		return nil, fmt.Errorf("giteareader: list PRs for %s: %w", repo, err)
	}
	prs := make([]poller.PR, 0, len(raw))
	for _, pr := range raw {
		prs = append(prs, poller.PR{
			Number: pr.Number,
			URL:    pr.HTMLURL,
			Branch: pr.Head.Ref,
			State:  pr.State,
		})
	}
	return prs, nil
}

// CheckRepoAccess verifies the token can read the repo by fetching its
// metadata, mirroring the gh-CLI reader's `gh repo view`.
func (r *Reader) CheckRepoAccess(repo string) error {
	path := fmt.Sprintf("/api/v1/repos/%s", repo)
	if err := r.get(context.Background(), path, nil); err != nil {
		return fmt.Errorf("giteareader: repo access check for %s: %w", repo, err)
	}
	return nil
}

// ReadIssue fetches a single issue's detail and maps it into
// poller.IssueDetails.
//
// Field mapping vs the GitHub gh-CLI reader:
//   - Number, State (lower-cased), Body, Labels (names): direct.
//   - StateReason: Gitea's issue object has no equivalent of GitHub's
//     stateReason ("completed" / "not_planned"), so it is left empty.
//   - ClosedByLinkedPR: Gitea exposes no per-issue "closed by linked PR"
//     reference list comparable to GitHub's closedByPullRequestsReferences,
//     so this is left false. Close detection in the poller still works via
//     the issue dropping out of the open-issues list (poll close-detection),
//     which is provider-neutral.
func (r *Reader) ReadIssue(repo string, issueNum int) (poller.IssueDetails, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/issues/%d", repo, issueNum)
	var raw giteaIssue
	if err := r.get(context.Background(), path, &raw); err != nil {
		return poller.IssueDetails{}, fmt.Errorf("giteareader: read issue %s#%d: %w", repo, issueNum, err)
	}
	return poller.IssueDetails{
		Number: raw.Number,
		State:  strings.ToLower(raw.State),
		Body:   raw.Body,
		Labels: labelNames(raw.Labels),
	}, nil
}

// ReadIssueComments fetches an issue's comments (author / body / createdAt)
// in the same shape the gh-CLI reader produces. It is not part of
// poller.GHReader but is provided so the Gitea path is faithful to the
// comment data the GitHub reader exposes; createdAt is RFC3339-formatted to
// match ghadapter.CLI.ReadIssueComments.
func (r *Reader) ReadIssueComments(repo string, issueNum int) ([]IssueComment, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/issues/%d/comments", repo, issueNum)
	var raw []giteaComment
	if err := r.get(context.Background(), path, &raw); err != nil {
		return nil, fmt.Errorf("giteareader: read comments %s#%d: %w", repo, issueNum, err)
	}
	out := make([]IssueComment, 0, len(raw))
	for _, c := range raw {
		createdAt := ""
		if !c.CreatedAt.IsZero() {
			createdAt = c.CreatedAt.Format(time.RFC3339)
		}
		out = append(out, IssueComment{Author: c.User.Login, Body: c.Body, CreatedAt: createdAt})
	}
	return out, nil
}

// IssueComment mirrors the fields the GitHub reader exposes for an issue
// comment. It is defined here (rather than reusing internal/runtime's type)
// to keep giteareader free of a runtime import; callers that need the runtime
// shape can map field-for-field.
type IssueComment struct {
	Author    string
	Body      string
	CreatedAt string
}

func labelNames(labels []giteaLabel) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}
