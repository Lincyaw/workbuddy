// Package taskprep materialises a scheduling decision into an executable
// WorkerTask: it loads GitHub issue context, persists the task row, and
// hands the task off to the embedded-worker channel.
//
// It lives outside internal/router so the scheduling decision (which agent,
// is it gated) stays decoupled from context gathering, persistence, and
// filesystem prep (see issue #145 finding #8).
package taskprep

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/google/uuid"
)

// WorkerTask is the unit of work sent to an embedded Worker via channel.
// It is defined here (rather than in internal/router) because materialising
// it is the preparer's responsibility. internal/router re-exports it as a
// type alias so existing consumers keep compiling.
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

// IssueDataReader loads the minimal GitHub context needed to render an agent
// prompt. It is intentionally narrow so taskprep tests can use a fake.
type IssueDataReader interface {
	ReadIssueSummary(repo string, issueNum int) (title, body string, labels []string, err error)
	ReadIssueComments(repo string, issueNum int) ([]runtimepkg.IssueComment, error)
	ListRelatedPRs(repo string, issueNum int) ([]runtimepkg.PRSummary, error)
}

// Decision is what the scheduling layer hands to the preparer. It carries
// enough identity to persist a task row and render an agent prompt, but has
// no GitHub or filesystem side-effects of its own.
type Decision struct {
	Repo      string
	IssueNum  int
	AgentName string
	Agent     *config.AgentConfig
	Workflow  string
	State     string
}

// TaskStore is the narrow persistence surface the preparer needs.
type TaskStore interface {
	InsertTask(rec store.TaskRecord) error
}

// Preparer turns a Decision into a dispatched WorkerTask.
//
// Responsibilities:
//   - persist a Task row in the store
//   - (if dispatching to an embedded worker) load GH context + push the
//     WorkerTask onto the supplied channel
//
// Workspace / worktree provisioning is intentionally NOT here — it lives on
// the worker side (internal/worker/executor.go) so transport-level dispatch
// does not do filesystem work.
type Preparer struct {
	store              TaskStore
	repoRoot           string
	taskChan           chan<- WorkerTask
	dispatchToEmbedded bool
	gh                 IssueDataReader
}

// NewPreparer constructs a Preparer. Pass dispatchToEmbedded=false for
// coordinator-only mode: the task row is persisted but the in-process
// channel is not used (a remote worker will claim it instead).
func NewPreparer(
	st TaskStore,
	repoRoot string,
	taskChan chan<- WorkerTask,
	dispatchToEmbedded bool,
) *Preparer {
	return &Preparer{
		store:              st,
		repoRoot:           repoRoot,
		taskChan:           taskChan,
		dispatchToEmbedded: dispatchToEmbedded,
		gh:                 ghadapter.NewCLI(),
	}
}

// SetIssueDataReader replaces the default GitHub issue-context reader.
// Passing nil restores the default CLI-backed reader.
func (p *Preparer) SetIssueDataReader(reader IssueDataReader) {
	if reader == nil {
		p.gh = ghadapter.NewCLI()
		return
	}
	p.gh = reader
}

// Prepare persists the task and, when dispatching to the embedded worker,
// loads GitHub context and pushes the WorkerTask onto the channel.
//
// Context cancellation during the channel send returns silently — this
// matches the legacy router behaviour where shutdown races are not treated
// as dispatch failures.
func (p *Preparer) Prepare(ctx context.Context, d Decision) error {
	if d.Agent == nil {
		return fmt.Errorf("taskprep: nil agent for %s#%d", d.Repo, d.IssueNum)
	}
	taskID := uuid.New().String()

	if err := p.store.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      d.Repo,
		IssueNum:  d.IssueNum,
		AgentName: d.AgentName,
		Role:      d.Agent.Role,
		Runtime:   d.Agent.Runtime,
		Workflow:  d.Workflow,
		State:     d.State,
		Status:    store.TaskStatusPending,
	}); err != nil {
		return fmt.Errorf("taskprep: insert task: %w", err)
	}

	if !p.dispatchToEmbedded || p.taskChan == nil {
		return nil
	}

	issueCtx := runtimepkg.IssueContext{Number: d.IssueNum}
	var relatedPRs []runtimepkg.PRSummary
	if p.gh != nil {
		if title, body, labels, err := p.gh.ReadIssueSummary(d.Repo, d.IssueNum); err != nil {
			log.Printf("[taskprep] warning: could not fetch issue details for %s#%d: %v", d.Repo, d.IssueNum, err)
		} else {
			issueCtx.Title = title
			issueCtx.Body = body
			issueCtx.Labels = labels
		}
		if comments, err := p.gh.ReadIssueComments(d.Repo, d.IssueNum); err != nil {
			log.Printf("[taskprep] warning: could not fetch issue comments for %s#%d: %v", d.Repo, d.IssueNum, err)
		} else {
			issueCtx.Comments = comments
			issueCtx.CommentsText = FormatComments(comments)
		}
		prs, err := p.gh.ListRelatedPRs(d.Repo, d.IssueNum)
		if err != nil {
			log.Printf("[taskprep] warning: could not fetch related PRs for %s#%d: %v", d.Repo, d.IssueNum, err)
		} else {
			relatedPRs = prs
		}
	}

	taskCtx := &runtimepkg.TaskContext{
		Issue:          issueCtx,
		Repo:           d.Repo,
		RepoRoot:       p.repoRoot,
		WorkDir:        p.repoRoot,
		RelatedPRs:     relatedPRs,
		RelatedPRsText: FormatRelatedPRs(relatedPRs),
		Session: runtimepkg.SessionContext{
			ID: fmt.Sprintf("session-%s-%s", taskID, uuid.New().String()[:8]),
		},
	}

	task := WorkerTask{
		TaskID:    taskID,
		Repo:      d.Repo,
		IssueNum:  d.IssueNum,
		AgentName: d.AgentName,
		Agent:     d.Agent,
		Context:   taskCtx,
		Workflow:  d.Workflow,
		State:     d.State,
	}

	select {
	case p.taskChan <- task:
	case <-ctx.Done():
	}
	return nil
}

// FormatComments renders issue comments into a human-readable blob for
// agent prompt templates. Exported so callers that build their own
// TaskContext can reuse the same format.
func FormatComments(comments []runtimepkg.IssueComment) string {
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

// FormatRelatedPRs renders related PR summaries into a human-readable blob
// for agent prompt templates.
func FormatRelatedPRs(prs []runtimepkg.PRSummary) string {
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
