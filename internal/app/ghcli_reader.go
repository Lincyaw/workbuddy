package app

import (
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/poller"
)

// IssueLabelReader exposes the GitHub label-read capability needed by the
// embedded worker and the router. It is a subset of the full GH adapter.
type IssueLabelReader interface {
	ReadIssueLabels(repo string, issueNum int) ([]string, error)
}

// GHCLIReader implements poller.GHReader (and IssueLabelReader) using the
// shared gh CLI adapter.
type GHCLIReader struct {
	Client *ghadapter.CLI
}

func (g *GHCLIReader) cli() *ghadapter.CLI {
	if g != nil && g.Client != nil {
		return g.Client
	}
	return ghadapter.NewCLI()
}

func (g *GHCLIReader) ListIssues(repo string) ([]poller.Issue, error) {
	return g.cli().ListIssues(repo)
}

func (g *GHCLIReader) ListPRs(repo string) ([]poller.PR, error) {
	return g.cli().ListPRs(repo)
}

func (g *GHCLIReader) CheckRepoAccess(repo string) error {
	return g.cli().CheckRepoAccess(repo)
}

func (g *GHCLIReader) ReadIssueLabels(repo string, issueNum int) ([]string, error) {
	return g.cli().ReadIssueLabels(repo, issueNum)
}

func (g *GHCLIReader) ReadIssue(repo string, issueNum int) (poller.IssueDetails, error) {
	return g.cli().ReadIssue(repo, issueNum)
}
