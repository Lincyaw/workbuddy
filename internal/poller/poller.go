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

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghutil"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// ---------------------------------------------------------------------------
// Event type constants
// ---------------------------------------------------------------------------

// Event types emitted by the Poller.
const (
	EventIssueCreated   = "issue_created"
	EventLabelAdded     = "label_added"
	EventLabelRemoved   = "label_removed"
	EventPRCreated      = "pr_created"
	EventPRStateChanged = "pr_state_changed"
	EventIssueClosed    = "issue_closed"
	// EventPollCycleDone is emitted at the end of every successful poll cycle.
	// Consumers use it as a boundary signal — e.g. resetting per-cycle dedup
	// state — and MUST NOT treat it as a per-issue event (IssueNum is 0).
	EventPollCycleDone = "poll_cycle_done"
)

// ghListLimit is the maximum number of results returned by gh issue/pr list.
// When a poll returns this many results, the list may be truncated and close
// detection is skipped to avoid false positives.
const ghListLimit = 100

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
	Author string   `json:"author"`
}

// PR represents a GitHub pull request as returned by gh pr list --json.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"headRefName"`
	State  string `json:"state"`
}

type IssueDetails struct {
	Number           int
	State            string
	StateReason      string
	Body             string
	Labels           []string
	ClosedByLinkedPR bool
}

// ChangeEvent describes a detected change between two polls.
type ChangeEvent struct {
	Type     string // EventIssueCreated, EventLabelAdded, EventLabelRemoved, EventPRCreated, EventPRStateChanged
	Repo     string
	IssueNum int
	Labels   []string
	Detail   string // e.g., which label was added
	Author   string
}

// ---------------------------------------------------------------------------
// GHReader interface (mockable for testing)
// ---------------------------------------------------------------------------

// GHReader abstracts GitHub read operations via gh CLI.
type GHReader interface {
	ListIssues(repo string) ([]Issue, error)
	ListPRs(repo string) ([]PR, error)
	CheckRepoAccess(repo string) error
	ReadIssue(repo string, issueNum int) (IssueDetails, error)
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
	eventlog   EventRecorder
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
		eventlog:   nil,
		backoff:    0,
		maxBackoff: 15 * time.Minute,
	}
}

// EventRecorder receives lightweight event records from the poller.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// SetEventRecorder sets the optional event recorder. When nil, rate-limit events
// are still handled but not persisted.
func (p *Poller) SetEventRecorder(r EventRecorder) {
	p.eventlog = r
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

// poll performs a single poll cycle as a two-phase snapshot:
//
//  1. Collect phase — list issues + PRs and diff them against the cache
//     purely in memory, producing a list of pending events and a list of
//     pending cache mutations. No cache writes happen here.
//  2. Commit phase — once every GitHub read has succeeded, apply all cache
//     mutations and emit all pending events. Only at the end do we emit
//     EventPollCycleDone.
//
// If any GitHub read fails partway through phase 1 (e.g. `gh pr list`
// returns an error after `gh issue list` succeeded), we abort without
// touching the cache and without emitting EventPollCycleDone, so the next
// cycle will re-diff the same issues and no changes are silently lost. See
// issue #145 finding #3.
func (p *Poller) poll(ctx context.Context) {
	// --- Phase 1: collect everything in memory ---
	var pending []ChangeEvent
	var cacheOps []func() error

	// Issues.
	issues, err := p.gh.ListIssues(p.repo)
	if err != nil {
		if ghutil.IsRateLimit(err) {
			p.logRateLimitEvent("issues", err)
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
		evts, op, ok := p.planDiffIssue(iss)
		if !ok {
			// Cache query failed; treat as a phase-1 failure and abort
			// before touching anything so the next cycle can retry.
			return
		}
		pending = append(pending, evts...)
		if op != nil {
			cacheOps = append(cacheOps, op)
		}
	}

	// PRs.
	prs, err := p.gh.ListPRs(p.repo)
	if err != nil {
		if ghutil.IsRateLimit(err) {
			p.logRateLimitEvent("prs", err)
			p.applyBackoff()
		} else {
			log.Printf("[poller] error listing PRs for %s: %v", p.repo, err)
		}
		// Phase 1 failed: do NOT commit cache, do NOT emit events, do NOT
		// emit EventPollCycleDone. The next cycle will re-observe the same
		// issue changes and re-emit them.
		return
	}

	for _, pr := range prs {
		if ctx.Err() != nil {
			return
		}
		evts, op, ok := p.planDiffPR(pr)
		if !ok {
			return
		}
		pending = append(pending, evts...)
		if op != nil {
			cacheOps = append(cacheOps, op)
		}
	}

	// Closed/deleted issue detection.
	// Compare cached issue numbers against what we saw this poll. Issues in
	// cache but not in current results have been closed or deleted. Skip
	// when the result set may be truncated (gh --limit 100).
	if len(issues) >= ghListLimit {
		log.Printf("[poller] issue list may be truncated (%d results), skipping close detection", len(issues))
	} else {
		openIssueNums := make(map[int]bool, len(issues))
		for _, iss := range issues {
			openIssueNums[iss.Number] = true
		}
		openPRNums := make(map[int]bool, len(prs))
		for _, pr := range prs {
			openPRNums[pr.Number] = true
		}

		cachedNums, err := p.store.ListCachedIssueNums(p.repo)
		if err != nil {
			log.Printf("[poller] error listing cached issue nums for %s: %v", p.repo, err)
			return
		}

		for _, num := range cachedNums {
			if ctx.Err() != nil {
				return
			}
			if openIssueNums[num] || openPRNums[num] {
				continue
			}
			closedNum := num
			pending = append(pending, ChangeEvent{
				Type:     EventIssueClosed,
				Repo:     p.repo,
				IssueNum: closedNum,
				Detail:   "issue no longer in open issues list",
			})
			cacheOps = append(cacheOps, func() error {
				if err := p.store.DeleteIssueCache(p.repo, closedNum); err != nil {
					log.Printf("[poller] error deleting cache for closed issue %s#%d: %v", p.repo, closedNum, err)
					return err
				}
				return nil
			})
		}
	}

	// All GH reads succeeded. Safe to reset backoff now.
	p.ResetBackoff()

	// --- Phase 2: commit cache updates, then emit events + cycle-done. ---
	for _, op := range cacheOps {
		if ctx.Err() != nil {
			return
		}
		_ = op()
	}
	for _, ev := range pending {
		if ctx.Err() != nil {
			return
		}
		p.emit(ctx, ev)
	}
	p.emit(ctx, ChangeEvent{Type: EventPollCycleDone, Repo: p.repo})
}

// planDiffIssue computes pending change events and a deferred cache-write op
// for a live issue without mutating the cache. The ok return is false only
// when querying the cache itself fails — which we treat as a phase-1 failure
// so the whole cycle can be retried.
func (p *Poller) planDiffIssue(iss Issue) (events []ChangeEvent, cacheOp func() error, ok bool) {
	labelsJSON := labelsToJSON(iss.Labels)

	cached, err := p.store.QueryIssueCache(p.repo, iss.Number)
	if err != nil {
		log.Printf("[poller] error querying cache for %s#%d: %v", p.repo, iss.Number, err)
		return nil, nil, false
	}

	if cached == nil {
		events = append(events, ChangeEvent{
			Type:     EventIssueCreated,
			Repo:     p.repo,
			IssueNum: iss.Number,
			Labels:   iss.Labels,
			Detail:   iss.Title,
			Author:   iss.Author,
		})
	} else {
		oldLabels := labelsFromJSON(cached.Labels)
		added, removed := diffLabels(oldLabels, iss.Labels)
		for _, l := range added {
			events = append(events, ChangeEvent{
				Type:     EventLabelAdded,
				Repo:     p.repo,
				IssueNum: iss.Number,
				Labels:   iss.Labels,
				Detail:   l,
				Author:   iss.Author,
			})
		}
		for _, l := range removed {
			events = append(events, ChangeEvent{
				Type:     EventLabelRemoved,
				Repo:     p.repo,
				IssueNum: iss.Number,
				Labels:   iss.Labels,
				Detail:   l,
				Author:   iss.Author,
			})
		}
	}

	issCopy := iss
	cacheOp = func() error {
		if err := p.store.UpsertIssueCache(store.IssueCache{
			Repo:     p.repo,
			IssueNum: issCopy.Number,
			Labels:   labelsJSON,
			Body:     issCopy.Body,
			State:    strings.ToLower(issCopy.State),
		}); err != nil {
			log.Printf("[poller] error upserting cache for %s#%d: %v", p.repo, issCopy.Number, err)
			return err
		}
		return nil
	}
	return events, cacheOp, true
}

// planDiffPR computes pending change events and a deferred cache-write op
// for a live PR without mutating the cache.
func (p *Poller) planDiffPR(pr PR) (events []ChangeEvent, cacheOp func() error, ok bool) {
	// PR numbers are distinct from issue numbers on GitHub, but to be safe
	// we prefix the cached state with "pr:".
	stateVal := "pr:" + strings.ToLower(pr.State)

	cached, err := p.store.QueryIssueCache(p.repo, pr.Number)
	if err != nil {
		log.Printf("[poller] error querying cache for PR %s#%d: %v", p.repo, pr.Number, err)
		return nil, nil, false
	}

	if cached == nil {
		events = append(events, ChangeEvent{
			Type:     EventPRCreated,
			Repo:     p.repo,
			IssueNum: pr.Number,
			Detail:   pr.Branch,
		})
	} else if cached.State != stateVal {
		events = append(events, ChangeEvent{
			Type:     EventPRStateChanged,
			Repo:     p.repo,
			IssueNum: pr.Number,
			Detail:   fmt.Sprintf("%s -> %s", cached.State, stateVal),
		})
	}

	prCopy := pr
	cacheOp = func() error {
		if err := p.store.UpsertIssueCache(store.IssueCache{
			Repo:     p.repo,
			IssueNum: prCopy.Number,
			Labels:   "",
			Body:     "",
			State:    stateVal,
		}); err != nil {
			log.Printf("[poller] error upserting cache for PR %s#%d: %v", p.repo, prCopy.Number, err)
			return err
		}
		return nil
	}
	return events, cacheOp, true
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

func (p *Poller) logRateLimitEvent(scope string, err error) {
	if p.eventlog == nil || err == nil {
		return
	}
	p.eventlog.Log(eventlog.TypeRateLimit, p.repo, 0, map[string]any{
		"source": "poller",
		"scope":  scope,
		"error":  ghutil.RedactTokens(err.Error()),
	})
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
