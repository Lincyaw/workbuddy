// Package router dispatches agent tasks to available workers.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workspace"
	"github.com/google/uuid"
)

// WorkerTask is the unit of work sent to an embedded Worker via channel.
type WorkerTask struct {
	TaskID       string
	Repo         string
	IssueNum     int
	AgentName    string
	Agent        *config.AgentConfig
	Context      *launcher.TaskContext
	Workflow     string
	State        string
	WorktreePath string // path to isolated worktree, empty if isolation disabled
}

// Router receives DispatchRequests from the StateMachine and routes them
// to Workers. In v0.1.0 it sends tasks over a Go channel to the embedded Worker.
type Router struct {
	agents   map[string]*config.AgentConfig
	registry *registry.Registry
	store    *store.Store
	repo     string
	repoRoot string
	taskChan chan<- WorkerTask
	wsMgr    *workspace.Manager // nil = no workspace isolation
}

// NewRouter creates a Router for v0.1.0 channel-based dispatch.
// Pass nil for wsMgr to disable workspace isolation (agents use CWD).
func NewRouter(
	agents map[string]*config.AgentConfig,
	reg *registry.Registry,
	st *store.Store,
	repo string,
	repoRoot string,
	taskChan chan<- WorkerTask,
	wsMgr *workspace.Manager,
) *Router {
	return &Router{
		agents:   agents,
		registry: reg,
		store:    st,
		repo:     repo,
		repoRoot: repoRoot,
		taskChan: taskChan,
		wsMgr:    wsMgr,
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
	depState, err := r.store.QueryIssueDependencyState(req.Repo, req.IssueNum)
	if err != nil {
		log.Printf("[router] failed to query dependency state for %s#%d: %v", req.Repo, req.IssueNum, err)
		return
	}
	if depState != nil && (depState.Verdict == store.DependencyVerdictBlocked || depState.Verdict == store.DependencyVerdictNeedsHuman) {
		log.Printf("[router] blocked dispatch for %s#%d due to dependency verdict %q", req.Repo, req.IssueNum, depState.Verdict)
		return
	}

	taskID := uuid.New().String()

	// Record task in store
	if err := r.store.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      req.Repo,
		IssueNum:  req.IssueNum,
		AgentName: req.AgentName,
		Status:    store.TaskStatusPending,
	}); err != nil {
		log.Printf("[router] failed to insert task: %v", err)
		return
	}

	// Distributed coordinator mode persists pending tasks for HTTP workers and
	// does not need to render an in-process task payload yet.
	if r.taskChan == nil {
		return
	}

	// Fetch issue details via gh CLI for template rendering.
	issueCtx := launcher.IssueContext{Number: req.IssueNum}
	if title, body, labels, err := fetchIssueDetails(req.Repo, req.IssueNum); err != nil {
		log.Printf("[router] warning: could not fetch issue details: %v", err)
	} else {
		issueCtx.Title = title
		issueCtx.Body = body
		issueCtx.Labels = labels
	}
	if comments, err := fetchIssueComments(req.Repo, req.IssueNum); err != nil {
		log.Printf("[router] warning: could not fetch issue comments: %v", err)
	} else {
		issueCtx.Comments = comments
		issueCtx.CommentsText = formatComments(comments)
	}
	relatedPRs, err := fetchRelatedPRs(req.Repo, req.IssueNum)
	if err != nil {
		log.Printf("[router] warning: could not fetch related PRs: %v", err)
	}

	// Determine WorkDir: use an isolated worktree if workspace manager is set,
	// otherwise fall back to CWD.
	var workDir string
	var worktreePath string
	if r.wsMgr != nil {
		wt, err := r.wsMgr.Create(req.IssueNum, taskID)
		if err != nil {
			log.Printf("[router] failed to create worktree for issue #%d: %v, falling back to CWD", req.IssueNum, err)
			workDir, _ = os.Getwd()
		} else {
			workDir = wt
			worktreePath = wt
		}
	} else {
		workDir, _ = os.Getwd()
	}

	taskCtx := &launcher.TaskContext{
		Issue:          issueCtx,
		Repo:           req.Repo,
		RepoRoot:       r.repoRoot,
		WorkDir:        workDir,
		RelatedPRs:     relatedPRs,
		RelatedPRsText: formatRelatedPRs(relatedPRs),
		Session: launcher.SessionContext{
			ID: fmt.Sprintf("session-%s", taskID),
		},
	}

	task := WorkerTask{
		TaskID:       taskID,
		Repo:         req.Repo,
		IssueNum:     req.IssueNum,
		AgentName:    req.AgentName,
		Agent:        agent,
		Context:      taskCtx,
		Workflow:     req.Workflow,
		State:        req.State,
		WorktreePath: worktreePath,
	}

	select {
	case r.taskChan <- task:
		// Task status is updated to running by the worker when it actually
		// starts executing, so pending tasks queued in the channel buffer
		// don't appear as running.
	case <-ctx.Done():
		// Clean up worktree that was created but never dispatched.
		if worktreePath != "" && r.wsMgr != nil {
			if err := r.wsMgr.Remove(worktreePath); err != nil {
				log.Printf("[router] failed to clean up worktree on cancellation: %v", err)
			}
		}
		return
	}
}

// ghIssueDetail matches the JSON output of gh issue view --json.
type ghIssueDetail struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// fetchIssueDetails calls gh issue view to get issue title, body, and labels.
func fetchIssueDetails(repo string, issueNum int) (title, body string, labels []string, err error) {
	cmd := exec.Command("gh", "issue", "view",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--json", "title,body,labels",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", "", nil, fmt.Errorf("gh issue view: %w", err)
	}

	var detail ghIssueDetail
	if err := json.Unmarshal(out, &detail); err != nil {
		return "", "", nil, fmt.Errorf("gh issue view: parse: %w", err)
	}

	labels = make([]string, len(detail.Labels))
	for i, l := range detail.Labels {
		labels[i] = l.Name
	}
	return detail.Title, detail.Body, labels, nil
}

// ghIssueComments is the shape returned by `gh issue view --json comments`.
type ghIssueComments struct {
	Comments []struct {
		Author    struct{ Login string } `json:"author"`
		Body      string                 `json:"body"`
		CreatedAt string                 `json:"createdAt"`
	} `json:"comments"`
}

func fetchIssueComments(repo string, issueNum int) ([]launcher.IssueComment, error) {
	cmd := exec.Command("gh", "issue", "view",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--json", "comments",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue view comments: %w", err)
	}
	var parsed ghIssueComments
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("gh issue view comments: parse: %w", err)
	}
	result := make([]launcher.IssueComment, 0, len(parsed.Comments))
	for _, c := range parsed.Comments {
		result = append(result, launcher.IssueComment{
			Author:    c.Author.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return result, nil
}

func formatComments(comments []launcher.IssueComment) string {
	if len(comments) == 0 {
		return "(no comments)"
	}
	var b strings.Builder
	for i, c := range comments {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "[%s by %s]\n%s", c.CreatedAt, c.Author, c.Body)
	}
	return b.String()
}

type ghPRSummary struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	Title       string `json:"title"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	URL         string `json:"url"`
	IsDraft     bool   `json:"isDraft"`
}

func fetchRelatedPRs(repo string, issueNum int) ([]launcher.PRSummary, error) {
	cmd := exec.Command("gh", "pr", "list",
		"--repo", repo,
		"--state", "all",
		"--search", fmt.Sprintf("%d in:title,body", issueNum),
		"--json", "number,state,title,headRefName,baseRefName,url,isDraft",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var parsed []ghPRSummary
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("gh pr list: parse: %w", err)
	}
	result := make([]launcher.PRSummary, 0, len(parsed))
	for _, p := range parsed {
		result = append(result, launcher.PRSummary{
			Number:      p.Number,
			State:       p.State,
			Title:       p.Title,
			HeadRefName: p.HeadRefName,
			BaseRefName: p.BaseRefName,
			URL:         p.URL,
			IsDraft:     p.IsDraft,
		})
	}
	return result, nil
}

func formatRelatedPRs(prs []launcher.PRSummary) string {
	if len(prs) == 0 {
		return "(no related PRs)"
	}
	var b strings.Builder
	for i, p := range prs {
		if i > 0 {
			b.WriteByte('\n')
		}
		draft := ""
		if p.IsDraft {
			draft = " [draft]"
		}
		fmt.Fprintf(&b, "#%d [%s]%s %s (head: %s, base: %s) - %s",
			p.Number, p.State, draft, p.Title, p.HeadRefName, p.BaseRefName, p.URL)
	}
	return b.String()
}
