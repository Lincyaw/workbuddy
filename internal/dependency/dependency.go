// Package dependency parses `workbuddy.depends_on` blocks from Issue bodies,
// detects cycles, and computes a per-issue dispatch verdict (ready / blocked /
// needs_human / override). The Coordinator gates dispatch on the verdict and
// surfaces blocked-state purely via a 😕 emoji reaction (no managed comment).
package dependency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghutil"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
	"gopkg.in/yaml.v3"
)

const (
	// OverrideLabel, when present on an issue, forces the verdict to
	// "override" regardless of upstream dependency state. Parsed-only here:
	// no DB write side-effect tied to the label itself.
	OverrideLabel = "override:force-unblock"
	StatusBlocked = "status:blocked"
	StatusDone    = "status:done"
	StatusFailed  = "status:failed"
)

var yamlFenceRe = regexp.MustCompile("(?s)```yaml\\s*\n(.*?)```")
var fqIssueRefRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

type IssueReader interface {
	ListIssues(repo string) ([]poller.Issue, error)
	ReadIssue(repo string, issueNum int) (poller.IssueDetails, error)
}

type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

type ParsedDependency struct {
	Raw              string
	Repo             string
	IssueNum         int
	Status           string
	Normalized       string
	ParseErrorReason string
	Line             int
}

type ParsedDeclaration struct {
	Dependencies []ParsedDependency
	SourceHash   string
	HasBlock     bool
}

type ResolveResult struct {
	State store.IssueDependencyState
	Deps  []store.IssueDependency
}

type Resolver struct {
	store    *store.Store
	reader   IssueReader
	eventlog EventRecorder
	alertBus *alertbus.Bus
}

func NewResolver(st *store.Store, reader IssueReader, eventlog EventRecorder, alertBus *alertbus.Bus) *Resolver {
	return &Resolver{store: st, reader: reader, eventlog: eventlog, alertBus: alertBus}
}

func ParseDeclaration(repo, body string) ParsedDeclaration {
	matches := yamlFenceRe.FindAllStringSubmatchIndex(body, -1)
	for _, match := range matches {
		block := body[match[2]:match[3]]
		var raw struct {
			Workbuddy struct {
				DependsOn []string `yaml:"depends_on"`
			} `yaml:"workbuddy"`
		}
		if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
			continue
		}
		if raw.Workbuddy.DependsOn == nil {
			continue
		}
		blockStartLine := 1 + strings.Count(body[:match[2]], "\n")
		linesByRaw := dependencyEntryLines(block, blockStartLine)
		decl := ParsedDeclaration{HasBlock: true}
		seen := make(map[string]struct{})
		for _, dep := range raw.Workbuddy.DependsOn {
			parsed := normalizeDependency(repo, dep)
			if lines := linesByRaw[parsed.Raw]; len(lines) > 0 {
				parsed.Line = lines[0]
				linesByRaw[parsed.Raw] = lines[1:]
			}
			if parsed.Normalized != "" {
				if _, ok := seen[parsed.Normalized]; ok {
					continue
				}
				seen[parsed.Normalized] = struct{}{}
			}
			decl.Dependencies = append(decl.Dependencies, parsed)
		}
		sort.Slice(decl.Dependencies, func(i, j int) bool {
			if decl.Dependencies[i].Normalized != decl.Dependencies[j].Normalized {
				return decl.Dependencies[i].Normalized < decl.Dependencies[j].Normalized
			}
			if decl.Dependencies[i].Raw != decl.Dependencies[j].Raw {
				return decl.Dependencies[i].Raw < decl.Dependencies[j].Raw
			}
			return decl.Dependencies[i].Line < decl.Dependencies[j].Line
		})
		decl.SourceHash = hashStrings(block, dependenciesSignature(decl.Dependencies))
		return decl
	}
	return ParsedDeclaration{}
}

// EvaluateOpenIssues parses dependency declarations for every cached open
// issue, detects cycles, computes a verdict, persists `issue_dependencies`
// (only for refs that parsed successfully) and the verdict state, and emits
// events when verdicts change. It returns the issue numbers whose verdict
// changed from blocked/needs_human to ready/override, so the caller can
// invalidate the poller cache and trigger redispatch.
func (r *Resolver) EvaluateOpenIssues(ctx context.Context, repo string, graphVersion int64) ([]int, error) {
	issues, err := r.store.ListIssueCaches(repo)
	if err != nil {
		return nil, err
	}
	openIssues := make(map[int]poller.Issue, len(issues))
	for _, cached := range issues {
		if cached.State != "open" {
			continue
		}
		openIssues[cached.IssueNum] = poller.Issue{
			Number: cached.IssueNum,
			Body:   cached.Body,
			State:  cached.State,
			Labels: labelsFromJSON(cached.Labels),
		}
	}

	parsedDecls := make(map[int]ParsedDeclaration, len(openIssues))
	graph := make(map[int][]int)
	for num, issue := range openIssues {
		decl := ParseDeclaration(repo, issue.Body)
		parsedDecls[num] = decl
		if err := r.syncMalformedDependencyHazard(repo, num, decl); err != nil {
			return nil, err
		}
		deps := make([]store.IssueDependency, 0, len(decl.Dependencies))
		for _, dep := range decl.Dependencies {
			if dep.Repo == "" || dep.IssueNum <= 0 {
				continue
			}
			deps = append(deps, store.IssueDependency{
				Repo:              repo,
				IssueNum:          num,
				DependsOnRepo:     dep.Repo,
				DependsOnIssueNum: dep.IssueNum,
				SourceHash:        decl.SourceHash,
				Status:            dep.Status,
			})
			if dep.Status == store.DependencyStatusActive && dep.Repo == repo {
				graph[num] = append(graph[num], dep.IssueNum)
			}
		}
		if err := r.store.ReplaceIssueDependencies(repo, num, deps); err != nil {
			return nil, err
		}
	}

	cycles := detectCycles(graph)
	closedCache := make(map[int]poller.IssueDetails)
	var unblocked []int

	for num, issue := range openIssues {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		result := buildResolveResult(repo, issue, parsedDecls[num], graphVersion, cycles[num], openIssues, closedCache, r.reader, r.eventlog)
		prev, err := r.store.QueryIssueDependencyState(repo, num)
		if err != nil {
			return nil, err
		}
		if !parsedDecls[num].HasBlock && prev == nil {
			continue
		}
		if prev != nil && prev.ResumeLabel != "" && result.State.ResumeLabel == "" {
			result.State.ResumeLabel = prev.ResumeLabel
		}
		if err := r.store.UpsertIssueDependencyState(result.State); err != nil {
			return nil, err
		}
		verdictChanged := prev == nil || prev.Verdict != result.State.Verdict || prev.BlockedReasonHash != result.State.BlockedReasonHash || prev.OverrideActive != result.State.OverrideActive
		if verdictChanged && r.eventlog != nil {
			r.eventlog.Log(eventlog.TypeDependencyVerdictChanged, repo, num, map[string]any{
				"verdict":         result.State.Verdict,
				"override_active": result.State.OverrideActive,
				"graph_version":   graphVersion,
			})
			if prev != nil &&
				(prev.Verdict == store.DependencyVerdictBlocked || prev.Verdict == store.DependencyVerdictNeedsHuman) &&
				(result.State.Verdict == store.DependencyVerdictReady || result.State.Verdict == store.DependencyVerdictOverride) {
				unblocked = append(unblocked, num)
			}
		}
		if cycles[num] != nil && r.eventlog != nil {
			r.eventlog.Log(eventlog.TypeDependencyCycleDetected, repo, num, map[string]any{
				"cycle_path": cycles[num],
			})
			r.publishAlert(alertbus.KindDependencyCycleDetected, alertbus.SeverityError, repo, num, "", map[string]any{
				"cycle_path": cycles[num],
			})
		}
		if result.State.OverrideActive && r.eventlog != nil {
			r.eventlog.Log(eventlog.TypeDependencyOverrideActivated, repo, num, map[string]any{
				"resume_label": result.State.ResumeLabel,
			})
		}
	}

	return unblocked, nil
}

func (r *Resolver) publishAlert(eventKind string, severity alertbus.Severity, repo string, issueNum int, agentName string, payload map[string]any) {
	if r.alertBus == nil {
		return
	}
	r.alertBus.Publish(alertbus.AlertEvent{
		Kind:      eventKind,
		Severity:  severity,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: agentName,
		Timestamp: time.Now().Unix(),
		Payload:   payload,
	})
}

func buildResolveResult(
	repo string,
	issue poller.Issue,
	decl ParsedDeclaration,
	graphVersion int64,
	cyclePath []string,
	openIssues map[int]poller.Issue,
	closedCache map[int]poller.IssueDetails,
	reader IssueReader,
	eventRecorder EventRecorder,
) ResolveResult {
	reasons := make([]string, 0)
	verdict := store.DependencyVerdictReady
	overrideActive := hasLabel(issue.Labels, OverrideLabel)
	resumeLabel := findResumeLabel(issue.Labels, "")

	if len(cyclePath) > 0 {
		verdict = store.DependencyVerdictNeedsHuman
		reasons = append(reasons, "cycle:"+strings.Join(cyclePath, " -> "))
	}

	for _, dep := range decl.Dependencies {
		switch dep.Status {
		case store.DependencyStatusUnsupportedCrossRepo:
			verdict = store.DependencyVerdictNeedsHuman
			reasons = append(reasons, dep.Normalized+":unsupported-cross-repo")
		case store.DependencyStatusInvalid:
			verdict = store.DependencyVerdictNeedsHuman
			reasons = append(reasons, dep.Raw+":"+dep.ParseErrorReason)
		default:
			if depIssue, ok := openIssues[dep.IssueNum]; ok && dep.Repo == repo {
				status := classifyOpenDependency(depIssue.Labels)
				if status != "done" {
					if status == "failed" {
						reasons = append(reasons, dep.Normalized+":failed")
					} else {
						reasons = append(reasons, dep.Normalized+":open")
					}
					if verdict == store.DependencyVerdictReady {
						verdict = store.DependencyVerdictBlocked
					}
				}
			} else {
				detail, ok := closedCache[dep.IssueNum]
				if !ok && dep.Repo == repo {
					read, err := reader.ReadIssue(repo, dep.IssueNum)
					if err == nil {
						detail = read
						closedCache[dep.IssueNum] = detail
						ok = true
					} else {
						if ghutil.IsRateLimit(err) {
							log.Printf("[dependency] rate limit while reading %s#%d for %d: %v", dep.Repo, dep.IssueNum, issue.Number, ghutil.RedactTokens(err.Error()))
							if eventRecorder != nil {
								eventRecorder.Log(eventlog.TypeRateLimit, repo, issue.Number, map[string]any{
									"source": "dependency_resolver",
									"dep":    dep.Normalized,
									"error":  ghutil.RedactTokens(err.Error()),
								})
							}
						}
					}
				}
				if ok && detail.State == "closed" && detail.ClosedByLinkedPR {
					// done — no reason added
				} else if ok && detail.State == "closed" {
					reasons = append(reasons, dep.Normalized+":closed-without-linked-pr")
					if verdict == store.DependencyVerdictReady {
						verdict = store.DependencyVerdictBlocked
					}
				} else {
					verdict = store.DependencyVerdictNeedsHuman
					reasons = append(reasons, dep.Normalized+":unreadable")
				}
			}
		}
	}

	if overrideActive {
		verdict = store.DependencyVerdictOverride
	}

	reasonHash := hashStrings(reasons...)
	state := store.IssueDependencyState{
		Repo:              repo,
		IssueNum:          issue.Number,
		Verdict:           verdict,
		ResumeLabel:       resumeLabel,
		BlockedReasonHash: reasonHash,
		OverrideActive:    overrideActive,
		GraphVersion:      graphVersion,
		LastEvaluatedAt:   time.Now(),
	}
	return ResolveResult{State: state}
}

func normalizeDependency(repo, raw string) ParsedDependency {
	raw = strings.TrimSpace(raw)
	parsed := ParsedDependency{Raw: raw}
	canonical := normalizeDependencyRef(raw)
	switch {
	case strings.HasPrefix(canonical, "#"):
		var num int
		if _, err := fmt.Sscanf(canonical, "#%d", &num); err != nil || num <= 0 {
			parsed.Status = store.DependencyStatusInvalid
			parsed.ParseErrorReason = "invalid_issue_number"
			return parsed
		}
		parsed.Repo = repo
		parsed.IssueNum = num
		parsed.Status = store.DependencyStatusActive
	case fqIssueRefRe.MatchString(canonical):
		match := fqIssueRefRe.FindStringSubmatch(canonical)
		var num int
		if _, err := fmt.Sscanf(match[3], "%d", &num); err != nil || num <= 0 {
			parsed.Status = store.DependencyStatusInvalid
			parsed.ParseErrorReason = "invalid_repo_issue"
			return parsed
		}
		parsed.Repo = match[1] + "/" + match[2]
		parsed.IssueNum = num
		if parsed.Repo == repo {
			parsed.Status = store.DependencyStatusActive
		} else {
			parsed.Status = store.DependencyStatusUnsupportedCrossRepo
		}
	default:
		parsed.Status = store.DependencyStatusInvalid
		parsed.ParseErrorReason = "invalid_format"
	}
	if parsed.Repo != "" && parsed.IssueNum > 0 {
		parsed.Normalized = fmt.Sprintf("%s#%d", parsed.Repo, parsed.IssueNum)
	}
	return parsed
}

func normalizeDependencyRef(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, `\"`, `"`)
	raw = strings.ReplaceAll(raw, `\'`, `'`)
	raw = stripMatchingQuotes(raw)
	return strings.TrimSpace(raw)
}

func stripMatchingQuotes(raw string) string {
	if len(raw) < 2 {
		return raw
	}
	if raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	if raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

func dependencyEntryLines(block string, blockStartLine int) map[string][]int {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(block), &root); err != nil {
		return nil
	}
	if len(root.Content) == 0 {
		return nil
	}
	workbuddy := lookupMappingValue(root.Content[0], "workbuddy")
	if workbuddy == nil {
		return nil
	}
	dependsOn := lookupMappingValue(workbuddy, "depends_on")
	if dependsOn == nil || dependsOn.Kind != yaml.SequenceNode {
		return nil
	}
	lines := make(map[string][]int, len(dependsOn.Content))
	for _, item := range dependsOn.Content {
		if item.Kind != yaml.ScalarNode {
			continue
		}
		lines[item.Value] = append(lines[item.Value], blockStartLine+item.Line-1)
	}
	return lines
}

func lookupMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func invalidDependencies(deps []ParsedDependency) []ParsedDependency {
	out := make([]ParsedDependency, 0)
	for _, dep := range deps {
		if dep.Status == store.DependencyStatusInvalid {
			out = append(out, dep)
		}
	}
	return out
}

func (r *Resolver) syncMalformedDependencyHazard(repo string, issueNum int, decl ParsedDeclaration) error {
	invalid := invalidDependencies(decl.Dependencies)
	if len(invalid) == 0 {
		return r.clearPipelineHazardIfKind(repo, issueNum, store.HazardKindMalformedDependencyRef)
	}
	fingerprint := hashStrings("malformed_dependency_ref", decl.SourceHash, dependenciesSignature(invalid))
	changed, err := r.store.UpsertIssuePipelineHazard(store.PipelineHazard{
		Repo:        repo,
		IssueNum:    issueNum,
		Kind:        store.HazardKindMalformedDependencyRef,
		Fingerprint: fingerprint,
	})
	if err != nil {
		return fmt.Errorf("dependency: upsert malformed dependency hazard for %s#%d: %w", repo, issueNum, err)
	}
	if !changed || r.eventlog == nil {
		return nil
	}
	r.eventlog.Log(eventlog.TypeIssueDependencyUnentered, repo, issueNum, map[string]any{
		"reason":               "unparseable_ref",
		"depends_on":           invalidDependencyRefs(invalid),
		"invalid_dependencies": invalidDependencyPayload(invalid),
		"hint":                 invalidDependencyHint(invalid),
	})
	return nil
}

func (r *Resolver) clearPipelineHazardIfKind(repo string, issueNum int, kind string) error {
	prev, err := r.store.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil {
		return fmt.Errorf("dependency: query pipeline hazard for %s#%d: %w", repo, issueNum, err)
	}
	if prev == nil || prev.Kind != kind {
		return nil
	}
	if err := r.store.ClearIssuePipelineHazard(repo, issueNum); err != nil {
		return fmt.Errorf("dependency: clear pipeline hazard for %s#%d: %w", repo, issueNum, err)
	}
	return nil
}

func invalidDependencyRefs(invalid []ParsedDependency) []string {
	refs := make([]string, 0, len(invalid))
	for _, dep := range invalid {
		refs = append(refs, dep.Raw)
	}
	return refs
}

func invalidDependencyPayload(invalid []ParsedDependency) []map[string]any {
	payload := make([]map[string]any, 0, len(invalid))
	for _, dep := range invalid {
		entry := map[string]any{
			"raw":                dep.Raw,
			"parse_error_reason": dep.ParseErrorReason,
		}
		if dep.Line > 0 {
			entry["line"] = dep.Line
		}
		payload = append(payload, entry)
	}
	return payload
}

func invalidDependencyHint(invalid []ParsedDependency) string {
	if len(invalid) == 0 {
		return "edit the malformed depends_on entry so it uses `#<int>` or `owner/repo#<int>`"
	}
	dep := invalid[0]
	if dep.Line > 0 {
		return fmt.Sprintf("edit issue body line %d under `workbuddy.depends_on`: replace %q with `#<int>` or `owner/repo#<int>`", dep.Line, dep.Raw)
	}
	return fmt.Sprintf("edit the malformed `workbuddy.depends_on` entry %q so it uses `#<int>` or `owner/repo#<int>`", dep.Raw)
}

func detectCycles(graph map[int][]int) map[int][]string {
	type color int
	const (
		white color = iota
		gray
		black
	)
	colors := make(map[int]color, len(graph))
	stack := make([]int, 0)
	out := make(map[int][]string)
	var visit func(int)
	visit = func(node int) {
		colors[node] = gray
		stack = append(stack, node)
		for _, next := range graph[node] {
			switch colors[next] {
			case white:
				visit(next)
			case gray:
				path := extractCyclePath(stack, next)
				for _, n := range path {
					out[n] = formatCycle(path)
				}
			}
		}
		stack = stack[:len(stack)-1]
		colors[node] = black
	}
	keys := make([]int, 0, len(graph))
	for node := range graph {
		keys = append(keys, node)
	}
	sort.Ints(keys)
	for _, node := range keys {
		if colors[node] == white {
			visit(node)
		}
	}
	return out
}

func extractCyclePath(stack []int, target int) []int {
	start := 0
	for i, n := range stack {
		if n == target {
			start = i
			break
		}
	}
	path := append([]int(nil), stack[start:]...)
	path = append(path, target)
	return path
}

func formatCycle(path []int) []string {
	out := make([]string, len(path))
	for i, n := range path {
		out[i] = fmt.Sprintf("#%d", n)
	}
	return out
}

func classifyOpenDependency(labels []string) string {
	switch {
	case hasLabel(labels, StatusDone):
		return "done"
	case hasLabel(labels, StatusFailed):
		return "failed"
	default:
		return "open"
	}
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func findResumeLabel(labels []string, fallback string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, "status:") && label != StatusBlocked {
			return label
		}
	}
	return fallback
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func dependenciesSignature(deps []ParsedDependency) string {
	data, _ := json.Marshal(deps)
	return string(data)
}

func labelsFromJSON(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil
	}
	return labels
}
