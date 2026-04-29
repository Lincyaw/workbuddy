// Package validate inspects a workbuddy configuration directory and
// reports structural / semantic issues as Diagnostics with stable codes
// (e.g. WB-X003) so editors and CI can act on them.
package validate

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Diagnostic codes registered in this file.
const (
	// CodeUnknownAgent — workflow references an agent name that no
	// `.github/workbuddy/agents/*.md` file declares.
	CodeUnknownAgent = "WB-X001"

	// CodeAgentBasenameMismatch — agent file basename stem does not
	// match its frontmatter `name:` (e.g. `dev-agent.md` with `name: dev`).
	CodeAgentBasenameMismatch = "WB-X002"

	// CodeUnknownRuntime — `runtime:` value not in the registered set.
	CodeUnknownRuntime = "WB-X003"

	// CodeUnknownRole — `role:` value not in {dev, review}.
	CodeUnknownRole = "WB-X004"

	// CodeDuplicateEnterLabel — two states in one workflow share the
	// same enter_label.
	CodeDuplicateEnterLabel = "WB-X005"

	// CodeUnknownTriggerState — agent's `triggers[].state:` references a
	// state name that is not present in any loaded workflow. Replaces the
	// removed WB-X006 (which keyed off the legacy label-based trigger).
	CodeUnknownTriggerState = "WB-X007"
)

// ValidRuntimes is the canonical set of runtime identifiers accepted in
// agent frontmatter. Mirrors the runtimes registered in
// internal/runtime/* — keep this list in sync when a new runtime lands.
var ValidRuntimes = map[string]struct{}{
	"codex":            {},
	"claude-code":      {},
	"claude-agent-sdk": {},
}

// ValidRoles is the canonical 2-role catalog. The project deliberately
// keeps the role set tiny (see CLAUDE.md / docs/decisions/2026-04-15-agent-role-consolidation.md).
var ValidRoles = map[string]struct{}{
	"dev":    {},
	"review": {},
}

// validateAgentCrossRefs runs WB-X002, WB-X003, WB-X004 on a single agent.
// The X001/X006 checks are still done in ValidateDir because they need the
// full agent+workflow set.
func validateAgentCrossRefs(agent *agentDoc) []Diagnostic {
	if agent == nil {
		return nil
	}
	var diags []Diagnostic

	// WB-X002 — basename stem vs frontmatter name. We pick the agent's
	// declared name as the source of truth; the file should match.
	stem := strings.TrimSuffix(filepath.Base(agent.Path), filepath.Ext(agent.Path))
	if agent.Name != "" && stem != "" && stem != agent.Name {
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     agent.NameLine,
			Severity: SeverityWarning,
			Code:     CodeAgentBasenameMismatch,
			Message: fmt.Sprintf(
				"agent file %q basename stem %q does not match frontmatter name %q",
				filepath.Base(agent.Path), stem, agent.Name,
			),
		})
	}

	// WB-X003 — runtime value validity. An empty runtime is allowed
	// because internal/config/loader.go defaults to claude-code.
	if rt := strings.TrimSpace(agent.Runtime); rt != "" {
		if _, ok := ValidRuntimes[rt]; !ok {
			diags = append(diags, Diagnostic{
				Path:     agent.Path,
				Line:     orFallback(agent.RuntimeLine, agent.NameLine),
				Severity: SeverityError,
				Code:     CodeUnknownRuntime,
				Message: fmt.Sprintf(
					"agent %q declares unknown runtime %q (valid: %s)",
					agent.Name, rt, sortedRuntimeList(),
				),
			})
		}
	}

	// WB-X004 — role validity.
	if role := strings.TrimSpace(agent.Role); role != "" {
		if _, ok := ValidRoles[role]; !ok {
			diags = append(diags, Diagnostic{
				Path:     agent.Path,
				Line:     orFallback(agent.RoleLine, agent.NameLine),
				Severity: SeverityError,
				Code:     CodeUnknownRole,
				Message: fmt.Sprintf(
					"agent %q declares unknown role %q (valid: dev, review)",
					agent.Name, role,
				),
			})
		}
	}

	return diags
}

// validateEnterLabelUniqueness implements WB-X005 — within a single
// workflow, two states sharing the same `enter_label:` is always a bug
// because the state machine looks up the next state by label.
func validateEnterLabelUniqueness(wf *workflowDoc) []Diagnostic {
	if wf == nil {
		return nil
	}
	type firstHit struct {
		state string
		line  int
	}
	seen := make(map[string]firstHit)
	// Walk in declared order so duplicates always blame the *second* one
	// and the first-seen line ends up in the message.
	var diags []Diagnostic
	for _, name := range wf.StateOrder {
		state := wf.States[name]
		if state == nil {
			continue
		}
		label := strings.TrimSpace(state.EnterLabel)
		if label == "" {
			continue
		}
		if prev, ok := seen[label]; ok {
			diags = append(diags, Diagnostic{
				Path:     wf.Path,
				Line:     orFallback(state.EnterLabelLine, state.Line),
				Severity: SeverityError,
				Code:     CodeDuplicateEnterLabel,
				Message: fmt.Sprintf(
					"workflow %q states %q and %q share enter_label %q (first seen at line %d)",
					wf.Name, prev.state, name, label, prev.line,
				),
			})
			continue
		}
		seen[label] = firstHit{state: name, line: orFallback(state.EnterLabelLine, state.Line)}
	}
	return diags
}

func orFallback(line, fallback int) int {
	if line > 0 {
		return line
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

func sortedRuntimeList() string {
	names := make([]string, 0, len(ValidRuntimes))
	for name := range ValidRuntimes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
