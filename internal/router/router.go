// Package router dispatches agent tasks to available workers.
package router

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
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
	Context      *runtimepkg.TaskContext
	Workflow     string
	State        string
	WorktreePath string // path to isolated worktree, empty if isolation disabled
}

type IssueDataReader interface {
	ReadIssueSummary(repo string, issueNum int) (title, body string, labels []string, err error)
	ReadIssueComments(repo string, issueNum int) ([]runtimepkg.IssueComment, error)
	ListRelatedPRs(repo string, issueNum int) ([]runtimepkg.PRSummary, error)
}

// Router receives DispatchRequests from the StateMachine and routes them
// to Workers. In v0.1.0 it sends tasks over a Go channel to the embedded Worker.
type Router struct {
	agents             map[string]*config.AgentConfig
	registry           *registry.Registry
	store              *store.Store
	repo               string
	repoRoot           string
	taskChan           chan<- WorkerTask
	wsMgr              *workspace.Manager // nil = no workspace isolation
	dispatchToEmbedded bool
	reporter           *reporter.Reporter
	gh                 IssueDataReader
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
	dispatchToEmbedded bool,
) *Router {
	return &Router{
		agents:             agents,
		registry:           reg,
		store:              st,
		repo:               repo,
		repoRoot:           repoRoot,
		taskChan:           taskChan,
		wsMgr:              wsMgr,
		dispatchToEmbedded: dispatchToEmbedded,
		gh:                 ghadapter.NewCLI(),
	}
}

// SetReporter sets the optional reporter used to post worktree failure comments.
func (r *Router) SetReporter(rep *reporter.Reporter) {
	r.reporter = rep
}

// SetIssueDataReader replaces the default GitHub issue-context reader.
func (r *Router) SetIssueDataReader(reader IssueDataReader) {
	if reader == nil {
		r.gh = ghadapter.NewCLI()
		return
	}
	r.gh = reader
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
		Role:      agent.Role,
		Runtime:   agent.Runtime,
		Workflow:  req.Workflow,
		State:     req.State,
		Status:    store.TaskStatusPending,
	}); err != nil {
		log.Printf("[router] failed to insert task: %v", err)
		return
	}

	if !r.dispatchToEmbedded || r.taskChan == nil {
		return
	}

	// Fetch issue details via the shared GitHub adapter for template rendering.
	issueCtx := runtimepkg.IssueContext{Number: req.IssueNum}
	if r.gh != nil {
		if title, body, labels, err := r.gh.ReadIssueSummary(req.Repo, req.IssueNum); err != nil {
			log.Printf("[router] warning: could not fetch issue details: %v", err)
		} else {
			issueCtx.Title = title
			issueCtx.Body = body
			issueCtx.Labels = labels
		}
		if comments, err := r.gh.ReadIssueComments(req.Repo, req.IssueNum); err != nil {
			log.Printf("[router] warning: could not fetch issue comments: %v", err)
		} else {
			issueCtx.Comments = comments
			issueCtx.CommentsText = formatComments(comments)
		}
	}
	var relatedPRs []runtimepkg.PRSummary
	if r.gh != nil {
		var err error
		relatedPRs, err = r.gh.ListRelatedPRs(req.Repo, req.IssueNum)
		if err != nil {
			log.Printf("[router] warning: could not fetch related PRs: %v", err)
		}
	}

	var repoRoot string
	var workDir string
	if r.wsMgr == nil {
		repoRoot = r.repoRoot
		workDir, _ = os.Getwd()
	} else {
		// The worker executor owns worktree setup/cleanup so transport dispatch
		// only carries the base repo path.
		repoRoot = r.repoRoot
		workDir = r.repoRoot
	}

	taskCtx := &runtimepkg.TaskContext{
		Issue:          issueCtx,
		Repo:           req.Repo,
		RepoRoot:       repoRoot,
		WorkDir:        workDir,
		RelatedPRs:     relatedPRs,
		RelatedPRsText: formatRelatedPRs(relatedPRs),
		Session: runtimepkg.SessionContext{
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
	}

	select {
	case r.taskChan <- task:
		// Task status is updated to running by the worker when it actually
		// starts executing, so pending tasks queued in the channel buffer
		// don't appear as running.
	case <-ctx.Done():
		return
	}
}

func formatComments(comments []runtimepkg.IssueComment) string {
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

func formatRelatedPRs(prs []runtimepkg.PRSummary) string {
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
