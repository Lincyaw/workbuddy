package validate

import (
	"fmt"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/Lincyaw/workbuddy/internal/runtime"
)

// Diagnostic codes emitted by the template-field check.
const (
	// CodePromptParseError — the agent's prompt template has a syntax
	// error and won't render at runtime.
	CodePromptParseError = "WB-T001"

	// CodeUnknownTemplateField — a `{{.Foo.Bar}}` expression refers to
	// a path that does not exist in TaskContext.
	CodeUnknownTemplateField = "WB-T101"

	// CodeContextFieldUndeclared — the prompt body references a TaskContext
	// field path that is not listed in the agent's `context:` declaration.
	// "Explicitly declare your inputs" enforcement.
	CodeContextFieldUndeclared = "WB-CT002"

	// CodeContextFieldUnused — the agent declares a `context:` entry that
	// is not referenced anywhere in the prompt body. Warning, not error;
	// dead declarations rot fast.
	CodeContextFieldUnused = "WB-CT003"
)

// validateAgentTemplate parses the agent's prompt body (markdown body of the
// agent file in the issue #204 batch 2 format) as a Go text/template, walks
// the parsed tree to find every dotted field reference, and reports unknown
// paths against `schema`. Also drives the context-coverage diff
// (WB-CT002/WB-CT003) using the same parse tree.
//
// Parse errors short-circuit and emit a single WB-T001.
func validateAgentTemplate(agent *agentDoc, schema FieldSchema) []Diagnostic {
	if agent == nil {
		return nil
	}
	prompt := agent.Prompt
	if strings.TrimSpace(prompt) == "" {
		// Empty prompt: no template work, but the context-coverage diff
		// still flags any declared-but-unused entries (WB-CT003).
		return validateContextCoverage(agent, map[string]struct{}{}, schemaPathSet(schema))
	}

	tmpl, err := template.New(agent.Name).Funcs(runtime.TemplateFuncMap()).Parse(prompt)
	if err != nil {
		return []Diagnostic{{
			Path:     agent.Path,
			Line:     agent.PromptLine,
			Severity: SeverityError,
			Code:     CodePromptParseError,
			Message:  fmt.Sprintf("agent %q prompt template parse error: %v", agent.Name, err),
		}}
	}

	refs := collectFieldRefs(tmpl)

	known := schemaPathSet(schema)
	candidates := schema.SortedPaths()

	// Build the set of fields the prompt actually references for the
	// context-coverage diff (WB-CT002 / WB-CT003).
	usedPaths := make(map[string]struct{}, len(refs))

	var diags []Diagnostic
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ref.path == "" {
			continue
		}
		usedPaths[ref.path] = struct{}{}
		if _, ok := known[ref.path]; ok {
			continue
		}
		// De-dupe identical (path,line) pairs so the same expression
		// repeated three times in a prompt only fires one diagnostic.
		key := fmt.Sprintf("%s\x00%d", ref.path, ref.line)
		if seen[key] {
			continue
		}
		seen[key] = true

		msg := fmt.Sprintf("agent %q template references unknown field {{.%s}}", agent.Name, ref.path)
		if hint := bestMatch(ref.path, candidates); hint != "" {
			msg += fmt.Sprintf(" (did you mean {{.%s}}?)", hint)
		}
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     promptLineFor(agent.PromptLine, ref.line),
			Severity: SeverityError,
			Code:     CodeUnknownTemplateField,
			Message:  msg,
		})
	}

	// WB-CT002 / WB-CT003 — context coverage diff.
	diags = append(diags, validateContextCoverage(agent, usedPaths, known)...)

	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Line != diags[j].Line {
			return diags[i].Line < diags[j].Line
		}
		return diags[i].Message < diags[j].Message
	})
	return diags
}

// validateContextCoverage computes the set diff between the declared `context:`
// list and the field paths the prompt actually references.
//
//   - Path used in prompt but not declared in `context:` → WB-CT002 (error).
//   - Path declared in `context:` but never used in prompt → WB-CT003 (warning).
//
// Iteration variants like `Issue.Comments[].Author` are normalized to their
// declared form in two ways: a context entry of `Issue.Comments[].Author`
// matches the iterator scope, and an entry of `Issue.Comments` covers any
// `Issue.Comments[].*` reference (declaring the whole slice covers its
// elements).
func validateContextCoverage(agent *agentDoc, usedPaths, known map[string]struct{}) []Diagnostic {
	if agent == nil {
		return nil
	}
	if len(agent.Context) == 0 && len(usedPaths) == 0 {
		// Nothing declared and nothing used — handled elsewhere (WB-CT001
		// fires on missing context; empty body fires WB-F002).
		return nil
	}

	declared := make(map[string]int, len(agent.Context))
	for i, entry := range agent.Context {
		declared[strings.TrimSpace(entry)] = i
	}

	declaredCovers := func(path string) bool {
		if _, ok := declared[path]; ok {
			return true
		}
		// `Issue.Comments` covers `Issue.Comments[].Foo`.
		for d := range declared {
			if d == "" {
				continue
			}
			if strings.HasPrefix(path, d+"[].") || strings.HasPrefix(path, d+".") {
				return true
			}
		}
		return false
	}

	usedCovers := func(decl string) bool {
		if _, ok := usedPaths[decl]; ok {
			return true
		}
		// A declaration of `Issue.Comments` is "used" if any `Issue.Comments[].*`
		// path appears, or any `Issue.Comments.*` path appears.
		for u := range usedPaths {
			if strings.HasPrefix(u, decl+"[].") || strings.HasPrefix(u, decl+".") {
				return true
			}
		}
		return false
	}

	var diags []Diagnostic

	// WB-CT002 — used but not declared. We only flag paths that exist in
	// the schema; unknown paths already produced WB-T101 above.
	usedSorted := make([]string, 0, len(usedPaths))
	for p := range usedPaths {
		usedSorted = append(usedSorted, p)
	}
	sort.Strings(usedSorted)
	seenUndeclared := make(map[string]struct{}, len(usedSorted))
	for _, p := range usedSorted {
		if _, ok := known[p]; !ok {
			continue
		}
		if declaredCovers(p) {
			continue
		}
		if _, dup := seenUndeclared[p]; dup {
			continue
		}
		seenUndeclared[p] = struct{}{}
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     orFallback(agent.ContextDeclLine, agent.PromptLine),
			Severity: SeverityError,
			Code:     CodeContextFieldUndeclared,
			Message: fmt.Sprintf(
				"agent %q prompt references {{.%s}} but %q is not declared in context:",
				agent.Name, p, p,
			),
		})
	}

	// WB-CT003 — declared but unused.
	for i, entry := range agent.Context {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if usedCovers(entry) {
			continue
		}
		line := agent.ContextDeclLine
		if i < len(agent.ContextLines) && agent.ContextLines[i] > 0 {
			line = agent.ContextLines[i]
		}
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     line,
			Severity: SeverityWarning,
			Code:     CodeContextFieldUnused,
			Message: fmt.Sprintf(
				"agent %q declares context: %q but the prompt never references it",
				agent.Name, entry,
			),
		})
	}

	return diags
}

// fieldRef captures a single template field expression and its line
// offset *inside the prompt body* (1-based, relative to the first body
// line).
type fieldRef struct {
	path string
	line int
}

// collectFieldRefs walks the parsed template tree to enumerate every
// .Foo.Bar reference. range/with blocks rebind the dot, so we maintain a
// scope stack: the topmost scope is the current iteration prefix.
func collectFieldRefs(tmpl *template.Template) []fieldRef {
	var refs []fieldRef
	for _, t := range tmpl.Templates() {
		if t == nil || t.Tree == nil || t.Root == nil {
			continue
		}
		walkNode(t.Root, []string{""}, &refs)
	}
	return refs
}

// walkNode recursively traverses a template parse tree. `scopeStack[len-1]`
// is the current "what does `.` resolve to" prefix — root scope is "",
// nested `range .Issue.Comments` pushes "Issue.Comments[]".
func walkNode(node parse.Node, scopeStack []string, refs *[]fieldRef) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			walkNode(child, scopeStack, refs)
		}
	case *parse.ActionNode:
		walkPipe(n.Pipe, scopeStack, refs)
	case *parse.RangeNode:
		// Resolve the iterator pipeline first under the current scope.
		newScope := topScope(scopeStack)
		if n.Pipe != nil {
			for _, cmd := range n.Pipe.Cmds {
				if cmd == nil {
					continue
				}
				if path, ok := commandFieldPath(cmd, topScope(scopeStack)); ok {
					*refs = append(*refs, fieldRef{path: path, line: pipeLine(n.Pipe)})
					// The iterator path is a slice; element scope
					// is path + "[]".
					newScope = path + "[]"
				}
			}
		}
		walkNode(n.List, append(scopeStack, newScope), refs)
		walkNode(n.ElseList, scopeStack, refs)
	case *parse.WithNode:
		newScope := topScope(scopeStack)
		if n.Pipe != nil {
			for _, cmd := range n.Pipe.Cmds {
				if cmd == nil {
					continue
				}
				if path, ok := commandFieldPath(cmd, topScope(scopeStack)); ok {
					*refs = append(*refs, fieldRef{path: path, line: pipeLine(n.Pipe)})
					newScope = path
				}
			}
		}
		walkNode(n.List, append(scopeStack, newScope), refs)
		walkNode(n.ElseList, scopeStack, refs)
	case *parse.IfNode:
		walkPipe(n.Pipe, scopeStack, refs)
		walkNode(n.List, scopeStack, refs)
		walkNode(n.ElseList, scopeStack, refs)
	case *parse.TemplateNode:
		walkPipe(n.Pipe, scopeStack, refs)
	}
}

func walkPipe(pipe *parse.PipeNode, scopeStack []string, refs *[]fieldRef) {
	if pipe == nil {
		return
	}
	for _, cmd := range pipe.Cmds {
		if cmd == nil {
			continue
		}
		if path, ok := commandFieldPath(cmd, topScope(scopeStack)); ok {
			*refs = append(*refs, fieldRef{path: path, line: pipe.Line})
		}
		// Walk additional arguments to catch nested fields like
		// {{shellEscape .Issue.Title}} — args[0] is already covered
		// by commandFieldPath above.
		for i := 1; i < len(cmd.Args); i++ {
			collectArgFieldRefs(cmd.Args[i], scopeStack, refs, pipe.Line)
		}
	}
}

func collectArgFieldRefs(node parse.Node, scopeStack []string, refs *[]fieldRef, line int) {
	switch n := node.(type) {
	case *parse.FieldNode:
		// FieldNode/ChainNode don't carry a Line; inherit the
		// enclosing pipe's line so diagnostics stay file-line.
		path := joinSchemaPath(topScope(scopeStack), n.Ident)
		if path != "" {
			*refs = append(*refs, fieldRef{path: path, line: line})
		}
	case *parse.ChainNode:
		path := chainPath(n, topScope(scopeStack))
		if path != "" {
			*refs = append(*refs, fieldRef{path: path, line: line})
		}
	case *parse.PipeNode:
		walkPipe(n, scopeStack, refs)
	}
}

// commandFieldPath returns the dotted path of a command's *first* argument
// when that argument is a FieldNode/ChainNode. Returns "" for funcs and
// literal arguments.
func commandFieldPath(cmd *parse.CommandNode, scope string) (string, bool) {
	if cmd == nil || len(cmd.Args) == 0 {
		return "", false
	}
	switch a := cmd.Args[0].(type) {
	case *parse.FieldNode:
		path := joinSchemaPath(scope, a.Ident)
		return path, path != ""
	case *parse.ChainNode:
		path := chainPath(a, scope)
		return path, path != ""
	case *parse.DotNode:
		return scope, scope != ""
	}
	return "", false
}

func chainPath(n *parse.ChainNode, scope string) string {
	if n == nil {
		return ""
	}
	// A ChainNode wraps a base node + suffixes (.Field). For our
	// purposes the base must itself be a FieldNode/DotNode/ChainNode;
	// anything else (e.g. function call) is opaque.
	var base string
	switch nb := n.Node.(type) {
	case *parse.FieldNode:
		base = joinSchemaPath(scope, nb.Ident)
	case *parse.DotNode:
		base = scope
	case *parse.ChainNode:
		base = chainPath(nb, scope)
	default:
		return ""
	}
	for _, f := range n.Field {
		base = joinSchemaPath(base, []string{f})
	}
	return base
}

func joinSchemaPath(scope string, idents []string) string {
	if len(idents) == 0 {
		return scope
	}
	parts := append([]string{}, idents...)
	if scope == "" {
		return strings.Join(parts, ".")
	}
	return scope + "." + strings.Join(parts, ".")
}

func topScope(stack []string) string {
	if len(stack) == 0 {
		return ""
	}
	return stack[len(stack)-1]
}

func pipeLine(p *parse.PipeNode) int {
	if p == nil {
		return 0
	}
	return p.Line
}

func schemaPathSet(s FieldSchema) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	return out
}

// promptLineFor maps a within-prompt template line back to an absolute
// file:line. Template line numbers are 1-based and refer to the input
// passed to Parse, so adding (promptLine - 1) gives the file-line.
func promptLineFor(promptLine, tmplLine int) int {
	if promptLine <= 0 {
		return tmplLine
	}
	if tmplLine <= 0 {
		return promptLine
	}
	return promptLine + tmplLine - 1
}

// bestMatch returns the closest candidate within Levenshtein distance ≤ 2
// (or the empty string when none qualifies). Lowercase comparison so
// `.repo` finds `.Repo`.
func bestMatch(target string, candidates []string) string {
	const maxDistance = 2
	best := ""
	bestDist := maxDistance + 1
	lt := strings.ToLower(target)
	for _, c := range candidates {
		// Skip iteration variants — they only make sense inside a
		// matching `range`; suggesting them blindly is misleading.
		if strings.Contains(c, "[]") {
			continue
		}
		d := levenshtein(lt, strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist <= maxDistance {
		return best
	}
	return ""
}

// levenshtein computes a tiny edit-distance with O(min(m,n)) memory. Pure
// inline implementation to avoid pulling in a dependency for two callers.
func levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			curr[j] = min3(del, ins, sub)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
