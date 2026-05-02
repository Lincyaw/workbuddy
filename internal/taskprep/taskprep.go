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
	"github.com/Lincyaw/workbuddy/internal/reporter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/google/uuid"
)

// WorkerTask is the unit of work sent to an embedded Worker via channel.
// It is defined here (rather than in internal/router) because materialising
// it is the preparer's responsibility. internal/router re-exports it as a
// type alias so existing consumers keep compiling.
type WorkerTask struct {
	TaskID         string
	Repo           string
	IssueNum       int
	AgentName      string
	Agent          *config.AgentConfig
	Context        *runtimepkg.TaskContext
	Workflow       string
	State          string
	RolloutIndex   int
	RolloutsTotal  int
	RolloutGroupID string
	WorktreePath   string // path to isolated worktree, empty if isolation disabled
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
	Repo           string
	IssueNum       int
	AgentName      string
	Agent          *config.AgentConfig
	Workflow       string
	State          string
	SourceState    string
	RolloutIndex   int
	RolloutsTotal  int
	RolloutGroupID string
	// StateDef is the workflow state object, attached so the preparer can
	// synthesize the transition footer (issue #204 batch 3) without reaching
	// back into the state-machine. May be nil for legacy callers; the
	// preparer treats a nil StateDef as "no footer".
	StateDef       *config.State
	SourceStateDef *config.State
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
		ID:             taskID,
		Repo:           d.Repo,
		IssueNum:       d.IssueNum,
		AgentName:      d.AgentName,
		Role:           d.Agent.Role,
		Runtime:        d.Agent.Runtime,
		Workflow:       d.Workflow,
		State:          d.State,
		RolloutIndex:   d.RolloutIndex,
		RolloutsTotal:  d.RolloutsTotal,
		RolloutGroupID: d.RolloutGroupID,
		Status:         store.TaskStatusPending,
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
			issueCtx.CommentsText = FormatComments(comments, d.Repo, d.IssueNum)
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
		Rollout: runtimepkg.RolloutContext{
			Index:   d.RolloutIndex,
			Total:   d.RolloutsTotal,
			GroupID: d.RolloutGroupID,
		},
		Session: runtimepkg.SessionContext{
			ID: fmt.Sprintf("session-%s-%s", taskID, uuid.New().String()[:8]),
		},
	}
	// Attach workflow-state metadata so the runtime can synthesize the
	// transition footer at prompt-render time. Terminal / nil states result
	// in no footer (BuildTransitionFooter returns "").
	if d.StateDef != nil {
		taskCtx.SetWorkflowState(d.State, d.StateDef.EnterLabel, d.StateDef.Transitions)
		taskCtx.SetWorkflowStateMode(d.StateDef.Mode)
	} else {
		taskCtx.SetWorkflowState(d.State, "", nil)
	}
	if synthStore, ok := p.store.(synthesisStore); ok {
		if synth, err := BuildSynthesisContext(d.Repo, d.IssueNum, d.Workflow, d.SourceState, d.StateDef, d.SourceStateDef, synthStore, p.gh, relatedPRs); err != nil {
			log.Printf("[taskprep] warning: could not build synthesis context for %s#%d: %v", d.Repo, d.IssueNum, err)
		} else {
			taskCtx.Synthesis = synth
		}
	}

	task := WorkerTask{
		TaskID:         taskID,
		Repo:           d.Repo,
		IssueNum:       d.IssueNum,
		AgentName:      d.AgentName,
		Agent:          d.Agent,
		Context:        taskCtx,
		Workflow:       d.Workflow,
		State:          d.State,
		RolloutIndex:   d.RolloutIndex,
		RolloutsTotal:  d.RolloutsTotal,
		RolloutGroupID: d.RolloutGroupID,
	}

	select {
	case p.taskChan <- task:
	case <-ctx.Done():
	}
	return nil
}

// reviewVerdictMarker is the HTML comment review-agent prepends to its
// criterion-by-criterion verdict so taskprep can locate the most recent
// verdict across an arbitrarily long dev↔review cycle history.
const reviewVerdictMarker = "<!-- workbuddy:review-verdict -->"

// maxVerdictBytes caps the quoted verdict body in the prompt. Verdicts
// past this length are truncated with a tail pointer to `gh issue view`
// so a runaway FAIL transcript doesn't bloat every subsequent dispatch.
// Sized for ~250 lines of typical AC output.
const maxVerdictBytes = 16 * 1024

// FormatComments renders issue comments into a compact, signal-dense blob
// for agent prompt templates. The strategy (issue #51 / REQ-061) is:
//
//  1. Drop reporter/bot noise (Agent Report, Cycle Cap, etc.).
//  2. Keep the most recent review verdict (identified by reviewVerdictMarker)
//     in full — that is the only comment guaranteed to be load-bearing for
//     the next dev iteration.
//  3. Replace every other surviving comment with a single counted summary
//     line, plus a `gh` invocation the agent can run on demand to read the
//     full history.
//
// Older issues without verdict markers fall back to the summary-only form;
// no verdict block is fabricated.
func FormatComments(comments []runtimepkg.IssueComment, repo string, issueNum int) string {
	if len(comments) == 0 {
		return "(no comments)"
	}

	noiseDropped := 0
	kept := make([]runtimepkg.IssueComment, 0, len(comments))
	for _, c := range comments {
		if reporter.IsAutomatedComment(c.Body) {
			noiseDropped++
			continue
		}
		kept = append(kept, c)
	}

	verdictIdx := -1
	for i := len(kept) - 1; i >= 0; i-- {
		if strings.Contains(kept[i].Body, reviewVerdictMarker) {
			verdictIdx = i
			break
		}
	}

	otherCount := len(kept)
	if verdictIdx >= 0 {
		otherCount--
	}

	var b strings.Builder
	if verdictIdx >= 0 {
		v := kept[verdictIdx]
		body := v.Body
		var tail string
		if len(body) > maxVerdictBytes {
			body = body[:maxVerdictBytes]
			tail = fmt.Sprintf("\n\n[... verdict body truncated at %d bytes; %d more bytes available via `gh issue view`]", maxVerdictBytes, len(v.Body)-maxVerdictBytes)
		}
		fmt.Fprintf(&b, "[Latest review verdict — %s by %s]\n%s%s", v.CreatedAt, v.Author, body, tail)
	}

	if otherCount > 0 || noiseDropped > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		ghCmd := "gh issue view"
		if issueNum > 0 {
			ghCmd += fmt.Sprintf(" %d", issueNum)
		}
		if repo != "" {
			ghCmd += fmt.Sprintf(" --repo %s", repo)
		}
		ghCmd += " --json comments"

		switch {
		case otherCount > 0 && noiseDropped > 0:
			fmt.Fprintf(&b, "[%d earlier comment(s) omitted; %d reporter/bot comment(s) filtered. Run `%s` to read the full history.]",
				otherCount, noiseDropped, ghCmd)
		case otherCount > 0:
			fmt.Fprintf(&b, "[%d earlier comment(s) omitted. Run `%s` to read the full history.]",
				otherCount, ghCmd)
		default: // noiseDropped > 0 only
			fmt.Fprintf(&b, "[%d reporter/bot comment(s) filtered as noise.]", noiseDropped)
		}
	}

	if b.Len() == 0 {
		return "(no comments)"
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
