package ghadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/poller"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

type RunCommand func(ctx context.Context, name string, args ...string) ([]byte, error)

type CLI struct {
	run         RunCommand
	runCombined RunCommand
}

type Comment struct {
	Author    string
	Body      string
	CreatedAt time.Time
}

type Reaction struct {
	ID      int64
	Content string
	User    string
}

type PullRequest struct {
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
}

func NewCLI() *CLI {
	return &CLI{run: defaultRunCommand, runCombined: defaultRunCombined}
}

func NewCLIWithRunner(run RunCommand) *CLI {
	if run == nil {
		return &CLI{run: defaultRunCommand, runCombined: defaultRunCombined}
	}
	return &CLI{run: run, runCombined: run}
}

func defaultRunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

func defaultRunCombined(ctx context.Context, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func (c *CLI) runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c == nil || c.run == nil {
		return defaultRunCommand(ctx, name, args...)
	}
	return c.run(ctx, name, args...)
}

func (c *CLI) runCommandCombined(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c == nil || c.runCombined == nil {
		return defaultRunCombined(ctx, name, args...)
	}
	return c.runCombined(ctx, name, args...)
}

func (c *CLI) ListIssues(repo string) ([]poller.Issue, error) {
	out, err := c.runCommand(nil, "gh", "issue", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,state,body,author,labels",
	)
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}

	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Body   string `json:"body"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue list: parse JSON: %w", err)
	}

	issues := make([]poller.Issue, len(raw))
	for i, r := range raw {
		labels := make([]string, len(r.Labels))
		for j, l := range r.Labels {
			labels[j] = l.Name
		}
		issues[i] = poller.Issue{Number: r.Number, Title: r.Title, State: r.State, Labels: labels, Body: r.Body, Author: r.Author.Login}
	}
	return issues, nil
}

func (c *CLI) ListPRs(repo string) ([]poller.PR, error) {
	out, err := c.runCommand(nil, "gh", "pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "100",
		"--json", "number,url,headRefName,state",
	)
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}

	var raw []struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr list: parse JSON: %w", err)
	}

	prs := make([]poller.PR, len(raw))
	for i, r := range raw {
		prs[i] = poller.PR{Number: r.Number, URL: r.URL, Branch: r.HeadRefName, State: r.State}
	}
	return prs, nil
}

func (c *CLI) CheckRepoAccess(repo string) error {
	out, err := c.runCommandCombined(nil, "gh", "repo", "view", repo, "--json", "name")
	if err != nil {
		output := strings.TrimSpace(string(out))
		if output != "" {
			return fmt.Errorf("ghadapter: gh repo view %s: %s: %w", repo, output, err)
		}
		return fmt.Errorf("ghadapter: gh repo view %s: %w", repo, err)
	}
	return nil
}

func (c *CLI) ReadIssueLabels(repo string, issueNum int) ([]string, error) {
	out, err := c.runCommand(nil, "gh", "issue", "view",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--json", "labels",
	)
	if err != nil {
		return nil, fmt.Errorf("gh issue view labels: %w", err)
	}
	var raw struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue view labels: parse JSON: %w", err)
	}
	labels := make([]string, len(raw.Labels))
	for i, l := range raw.Labels {
		labels[i] = l.Name
	}
	return labels, nil
}

func (c *CLI) ReadIssue(repo string, issueNum int) (poller.IssueDetails, error) {
	query := fmt.Sprintf(`query{repository(owner:%q,name:%q){issue(number:%d){number state stateReason body labels(first:100){nodes{name}} closedByPullRequestsReferences(first:10){nodes{number state url}}}}}`,
		repoOwner(repo), repoName(repo), issueNum)
	out, err := c.runCommand(nil, "gh", "api", "graphql", "-f", "query="+query)
	if err != nil {
		return poller.IssueDetails{}, fmt.Errorf("gh api graphql issue detail: %w", err)
	}

	var raw struct {
		Data struct {
			Repository struct {
				Issue struct {
					Number      int    `json:"number"`
					State       string `json:"state"`
					StateReason string `json:"stateReason"`
					Body        string `json:"body"`
					Labels      struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					ClosedByPullRequestsReferences struct {
						Nodes []struct {
							Number int    `json:"number"`
							State  string `json:"state"`
							URL    string `json:"url"`
						} `json:"nodes"`
					} `json:"closedByPullRequestsReferences"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return poller.IssueDetails{}, fmt.Errorf("gh api graphql issue detail parse: %w", err)
	}
	issue := raw.Data.Repository.Issue
	labels := make([]string, 0, len(issue.Labels.Nodes))
	for _, node := range issue.Labels.Nodes {
		labels = append(labels, node.Name)
	}
	closedByLinkedPR := false
	for _, pr := range issue.ClosedByPullRequestsReferences.Nodes {
		if strings.EqualFold(pr.State, "MERGED") || strings.EqualFold(pr.State, "CLOSED") {
			closedByLinkedPR = true
			break
		}
	}
	return poller.IssueDetails{
		Number:           issue.Number,
		State:            strings.ToLower(issue.State),
		StateReason:      strings.ToLower(issue.StateReason),
		Body:             issue.Body,
		Labels:           labels,
		ClosedByLinkedPR: closedByLinkedPR,
	}, nil
}

func (c *CLI) ReadIssueSummary(repo string, issueNum int) (title, body string, labels []string, err error) {
	out, err := c.runCommand(nil, "gh", "issue", "view",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--json", "title,body,labels",
	)
	if err != nil {
		return "", "", nil, fmt.Errorf("gh issue view: %w", err)
	}
	var raw struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", "", nil, fmt.Errorf("gh issue view: parse: %w", err)
	}
	labels = make([]string, len(raw.Labels))
	for i, l := range raw.Labels {
		labels[i] = l.Name
	}
	return raw.Title, raw.Body, labels, nil
}

func (c *CLI) ReadIssueComments(repo string, issueNum int) ([]runtimepkg.IssueComment, error) {
	comments, err := c.readComments(nil, "issue", repo, issueNum)
	if err != nil {
		return nil, err
	}
	out := make([]runtimepkg.IssueComment, 0, len(comments))
	for _, comment := range comments {
		createdAt := ""
		if !comment.CreatedAt.IsZero() {
			createdAt = comment.CreatedAt.Format(time.RFC3339)
		}
		out = append(out, runtimepkg.IssueComment{Author: comment.Author, Body: comment.Body, CreatedAt: createdAt})
	}
	return out, nil
}

func (c *CLI) ReadDetailedIssueComments(ctx context.Context, repo string, issueNum int) ([]Comment, error) {
	return c.readComments(ctx, "issue", repo, issueNum)
}

func (c *CLI) ReadPullRequestComments(ctx context.Context, repo string, prNum int) ([]Comment, error) {
	return c.readComments(ctx, "pr", repo, prNum)
}

func (c *CLI) readComments(ctx context.Context, kind, repo string, num int) ([]Comment, error) {
	out, err := c.runCommand(ctx, "gh", kind, "view",
		strconv.Itoa(num),
		"--repo", repo,
		"--json", "comments",
	)
	if err != nil {
		return nil, fmt.Errorf("gh %s view comments: %w", kind, err)
	}
	var raw struct {
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh %s view comments: parse: %w", kind, err)
	}
	comments := make([]Comment, 0, len(raw.Comments))
	for _, comment := range raw.Comments {
		comments = append(comments, Comment{Author: comment.Author.Login, Body: comment.Body, CreatedAt: comment.CreatedAt})
	}
	return comments, nil
}

func (c *CLI) ListRelatedPRs(repo string, issueNum int) ([]runtimepkg.PRSummary, error) {
	out, err := c.runCommand(nil, "gh", "pr", "list",
		"--repo", repo,
		"--state", "all",
		"--search", fmt.Sprintf("%d in:title,body", issueNum),
		"--json", "number,state,title,headRefName,baseRefName,url,isDraft",
	)
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var raw []struct {
		Number      int    `json:"number"`
		State       string `json:"state"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		URL         string `json:"url"`
		IsDraft     bool   `json:"isDraft"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr list: parse: %w", err)
	}
	prs := make([]runtimepkg.PRSummary, 0, len(raw))
	for _, pr := range raw {
		prs = append(prs, runtimepkg.PRSummary{Number: pr.Number, State: pr.State, Title: pr.Title, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName, URL: pr.URL, IsDraft: pr.IsDraft})
	}
	return prs, nil
}

func (c *CLI) WriteIssueComment(ctx context.Context, repo string, issueNum int, body string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "gh", "issue", "comment",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--body-file", "-",
	)
	cmd.Stdin = strings.NewReader(body)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghadapter: gh issue comment: %s: %w", string(output), err)
	}
	return nil
}

func (c *CLI) AuthenticatedLogin(ctx context.Context) (string, error) {
	out, err := c.runCommand(ctx, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("ghadapter: gh api user: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *CLI) AddIssueReaction(ctx context.Context, repo string, issueNum int, content string) error {
	out, err := c.runCommandCombined(ctx, "gh", "api", "-X", "POST",
		fmt.Sprintf("repos/%s/issues/%d/reactions", repo, issueNum),
		"-f", "content="+content,
		"-H", "Accept: application/vnd.github+json",
	)
	if err != nil {
		return fmt.Errorf("ghadapter: gh api POST reactions: %s: %w", string(out), err)
	}
	return nil
}

func (c *CLI) ListIssueReactions(ctx context.Context, repo string, issueNum int) ([]Reaction, error) {
	out, err := c.runCommand(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/issues/%d/reactions", repo, issueNum),
		"-H", "Accept: application/vnd.github+json",
	)
	if err != nil {
		return nil, fmt.Errorf("ghadapter: gh api GET reactions: %w", err)
	}
	var raw []struct {
		ID      int64  `json:"id"`
		Content string `json:"content"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("ghadapter: parse reactions: %w", err)
	}
	reactions := make([]Reaction, 0, len(raw))
	for _, reaction := range raw {
		reactions = append(reactions, Reaction{ID: reaction.ID, Content: reaction.Content, User: reaction.User.Login})
	}
	return reactions, nil
}

func (c *CLI) DeleteIssueReaction(ctx context.Context, repo string, issueNum int, reactionID int64) error {
	out, err := c.runCommandCombined(ctx, "gh", "api", "-X", "DELETE",
		fmt.Sprintf("repos/%s/issues/%d/reactions/%d", repo, issueNum, reactionID),
	)
	if err != nil {
		return fmt.Errorf("ghadapter: gh api DELETE reactions/%d: %s: %w", reactionID, string(out), err)
	}
	return nil
}

func (c *CLI) ReadPullRequest(ctx context.Context, repo string, prNum int) (PullRequest, error) {
	out, err := c.runCommand(ctx, "gh", "pr", "view", strconv.Itoa(prNum), "--repo", repo, "--json", "state,headRefName")
	if err != nil {
		return PullRequest{}, fmt.Errorf("gh pr view: %w", err)
	}
	var pr PullRequest
	if err := json.Unmarshal(out, &pr); err != nil {
		return PullRequest{}, fmt.Errorf("gh pr view: parse: %w", err)
	}
	return pr, nil
}

func repoOwner(repo string) string {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return repo
}

func repoName(repo string) string {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return repo
}
