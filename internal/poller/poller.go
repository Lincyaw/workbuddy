// Package poller periodically queries GitHub for issue and PR changes.
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Issue represents a GitHub issue as returned by gh issue list --json.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
	Body   string   `json:"body"`
}

// PR represents a GitHub pull request as returned by gh pr list --json.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"headRefName"`
	State  string `json:"state"`
}

// ChangeEvent describes a detected change between two polls.
type ChangeEvent struct {
	Type     string // "issue_created", "label_added", "label_removed", "pr_created", "pr_checks_changed", "pr_review_changed"
	Repo     string
	IssueNum int
	Labels   []string
	Detail   string // e.g., which label was added
}

// ---------------------------------------------------------------------------
// GHReader interface (mockable for testing)
// ---------------------------------------------------------------------------

// GHReader abstracts GitHub read operations via gh CLI.
type GHReader interface {
	ListIssues(repo string) ([]Issue, error)
	ListPRs(repo string) ([]PR, error)
	CheckRepoAccess(repo string) error
}

// ---------------------------------------------------------------------------
// Poller
// ---------------------------------------------------------------------------

// Poller periodically queries GitHub for issue/PR changes and emits events.
type Poller struct {
	gh         GHReader
	store      *store.Store
	repo       string
	interval   time.Duration
	events     chan ChangeEvent
	backoff    time.Duration
	maxBackoff time.Duration
}

// NewPoller creates a Poller with the given configuration.
// Default interval is 30s; events channel has a buffer of 256.
func NewPoller(gh GHReader, st *store.Store, repo string, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Poller{
		gh:         gh,
		store:      st,
		repo:       repo,
		interval:   interval,
		events:     make(chan ChangeEvent, 256),
		backoff:    0,
		maxBackoff: 15 * time.Minute,
	}
}

// Events returns the read-only channel of change events.
func (p *Poller) Events() <-chan ChangeEvent {
	return p.events
}

// PreCheck verifies that the gh CLI has access to the configured repo.
func (p *Poller) PreCheck() error {
	if err := p.gh.CheckRepoAccess(p.repo); err != nil {
		return fmt.Errorf("poller: pre-check failed for repo %s: %w", p.repo, err)
	}
	return nil
}

// Run starts the poll loop. It blocks until ctx is cancelled.
// On context cancellation it closes the events channel and returns nil.
func (p *Poller) Run(ctx context.Context) error {
	defer close(p.events)

	// Perform first poll immediately (full sync).
	p.poll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if p.backoff > 0 {
				log.Printf("[poller] rate-limit backoff active (%s remaining), skipping poll", p.backoff)
				p.backoff -= p.interval
				if p.backoff < 0 {
					p.backoff = 0
				}
				continue
			}
			p.poll(ctx)
		}
	}
}

// poll performs a single poll cycle: list issues + PRs, diff against cache, emit events.
func (p *Poller) poll(ctx context.Context) {
	// --- Issues ---
	issues, err := p.gh.ListIssues(p.repo)
	if err != nil {
		if isRateLimit(err) {
			p.applyBackoff()
		} else {
			log.Printf("[poller] error listing issues for %s: %v", p.repo, err)
		}
		return
	}

	for _, iss := range issues {
		if ctx.Err() != nil {
			return
		}
		p.diffIssue(ctx, iss)
	}

	// --- PRs ---
	prs, err := p.gh.ListPRs(p.repo)
	if err != nil {
		if isRateLimit(err) {
			p.applyBackoff()
		} else {
			log.Printf("[poller] error listing PRs for %s: %v", p.repo, err)
		}
		return
	}

	for _, pr := range prs {
		if ctx.Err() != nil {
			return
		}
		p.diffPR(ctx, pr)
	}
}

// diffIssue compares a live issue against the cache and emits change events.
func (p *Poller) diffIssue(ctx context.Context, iss Issue) {
	labelsJSON := labelsToJSON(iss.Labels)

	cached, err := p.store.QueryIssueCache(p.repo, iss.Number)
	if err != nil {
		log.Printf("[poller] error querying cache for %s#%d: %v", p.repo, iss.Number, err)
		return
	}

	if cached == nil {
		// New issue (or first sync after restart).
		p.emit(ctx, ChangeEvent{
			Type:     "issue_created",
			Repo:     p.repo,
			IssueNum: iss.Number,
			Labels:   iss.Labels,
			Detail:   iss.Title,
		})
	} else {
		// Compare labels.
		oldLabels := labelsFromJSON(cached.Labels)
		added, removed := diffLabels(oldLabels, iss.Labels)
		for _, l := range added {
			p.emit(ctx, ChangeEvent{
				Type:     "label_added",
				Repo:     p.repo,
				IssueNum: iss.Number,
				Labels:   iss.Labels,
				Detail:   l,
			})
		}
		for _, l := range removed {
			p.emit(ctx, ChangeEvent{
				Type:     "label_removed",
				Repo:     p.repo,
				IssueNum: iss.Number,
				Labels:   iss.Labels,
				Detail:   l,
			})
		}
	}

	// Update cache.
	if err := p.store.UpsertIssueCache(store.IssueCache{
		Repo:     p.repo,
		IssueNum: iss.Number,
		Labels:   labelsJSON,
		State:    iss.State,
	}); err != nil {
		log.Printf("[poller] error upserting cache for %s#%d: %v", p.repo, iss.Number, err)
	}
}

// diffPR compares a live PR against the cache and emits change events.
func (p *Poller) diffPR(ctx context.Context, pr PR) {
	// PRs are cached with negative number to avoid collision with issues.
	// Actually, PR numbers are distinct from issue numbers on GitHub, but
	// to be safe we use a "pr:" prefix in the state field.
	stateVal := "pr:" + pr.State

	cached, err := p.store.QueryIssueCache(p.repo, pr.Number)
	if err != nil {
		log.Printf("[poller] error querying cache for PR %s#%d: %v", p.repo, pr.Number, err)
		return
	}

	if cached == nil {
		p.emit(ctx, ChangeEvent{
			Type:     "pr_created",
			Repo:     p.repo,
			IssueNum: pr.Number,
			Detail:   pr.Branch,
		})
	} else if cached.State != stateVal {
		// Detect state changes (checks, reviews show as state changes).
		p.emit(ctx, ChangeEvent{
			Type:     "pr_state_changed",
			Repo:     p.repo,
			IssueNum: pr.Number,
			Detail:   fmt.Sprintf("%s -> %s", cached.State, stateVal),
		})
	}

	// Update cache.
	if err := p.store.UpsertIssueCache(store.IssueCache{
		Repo:     p.repo,
		IssueNum: pr.Number,
		Labels:   "",
		State:    stateVal,
	}); err != nil {
		log.Printf("[poller] error upserting cache for PR %s#%d: %v", p.repo, pr.Number, err)
	}
}

// emit sends a ChangeEvent on the events channel, respecting context cancellation.
func (p *Poller) emit(ctx context.Context, ev ChangeEvent) {
	select {
	case p.events <- ev:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------------------
// Rate limit / backoff
// ---------------------------------------------------------------------------

func isRateLimit(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "429")
}

func (p *Poller) applyBackoff() {
	if p.backoff == 0 {
		p.backoff = 60 * time.Second
	} else {
		p.backoff *= 2
	}
	if p.backoff > p.maxBackoff {
		p.backoff = p.maxBackoff
	}
	log.Printf("[poller] rate limit detected, backing off for %s", p.backoff)
}

// ResetBackoff resets the backoff timer (useful after a successful poll
// or for testing).
func (p *Poller) ResetBackoff() {
	p.backoff = 0
}

// Backoff returns the current backoff duration (for testing).
func (p *Poller) Backoff() time.Duration {
	return p.backoff
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func labelsToJSON(labels []string) string {
	if labels == nil {
		labels = []string{}
	}
	sorted := make([]string, len(labels))
	copy(sorted, labels)
	sort.Strings(sorted)
	b, _ := json.Marshal(sorted)
	return string(b)
}

func labelsFromJSON(s string) []string {
	if s == "" {
		return nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(s), &labels); err != nil {
		return nil
	}
	return labels
}

// diffLabels returns labels added and removed between old and new sets.
func diffLabels(old, newLabels []string) (added, removed []string) {
	oldSet := make(map[string]bool, len(old))
	for _, l := range old {
		oldSet[l] = true
	}
	newSet := make(map[string]bool, len(newLabels))
	for _, l := range newLabels {
		newSet[l] = true
	}

	for _, l := range newLabels {
		if !oldSet[l] {
			added = append(added, l)
		}
	}
	for _, l := range old {
		if !newSet[l] {
			removed = append(removed, l)
		}
	}
	return added, removed
}
