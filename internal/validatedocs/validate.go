package validatedocs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	intvalidate "github.com/Lincyaw/workbuddy/internal/validate"
	"gopkg.in/yaml.v3"
)

const (
	CodeMissingCodePath          = "WB-D001"
	CodeMissingTestPath          = "WB-D002"
	CodeMissingRelatedDocPath    = "WB-D003"
	CodeMissingSkillReference    = "WB-D004"
	CodePluginSyncDrift          = "WB-D101"
	CodeAgentFrontmatterMismatch = "WB-D201"
	CodeDuplicateSkillDivergence = "WB-D202"
	CodeSkillNameMismatch        = "WB-D301"
	CodeSkillDescriptionTooShort = "WB-D302"
	CodeSkillFlagEnumeration     = "WB-D303"
)

var skillReferenceRE = regexp.MustCompile(`references/[A-Za-z0-9._/-]+\.md`)
var skillFlagLineRE = regexp.MustCompile(`^\s*--[a-z-]+\b`)

type projectIndex struct {
	Requirements []projectRequirement `yaml:"requirements"`
}

type projectRequirement struct {
	ID          string   `yaml:"id"`
	Code        []string `yaml:"code"`
	Tests       []string `yaml:"tests"`
	RelatedDocs []string `yaml:"related_docs"`
}

// ValidateRepo validates documentation- and skill-oriented drift surfaces.
func ValidateRepo(root string) ([]intvalidate.Diagnostic, error) {
	return ValidateRepoWithOptions(root, Options{})
}

// Options controls optional validator behavior.
type Options struct {
	// SyncCheckCommand overrides the command used for WB-D101.
	// When nil, ValidateRepo runs `python3 scripts/sync_codex_plugin.py --check`.
	SyncCheckCommand []string
}

// ValidateRepoWithOptions validates repo-local doc drift surfaces.
func ValidateRepoWithOptions(root string, opts Options) ([]intvalidate.Diagnostic, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("validate-docs: repo root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("validate-docs: %q is not a directory", root)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("validate-docs: abs root: %w", err)
	}

	var diags []intvalidate.Diagnostic
	diags = append(diags, validateProjectIndex(absRoot)...)
	diags = append(diags, validateSkillReferencePaths(absRoot)...)
	syncDiags, err := validatePluginSync(absRoot, opts)
	if err != nil {
		return nil, err
	}
	diags = append(diags, syncDiags...)
	diags = append(diags, validateAgentSiblingParity(absRoot)...)
	diags = append(diags, validateSkillCopyParity(absRoot)...)
	diags = append(diags, validateSkillHygiene(absRoot)...)

	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		if diags[i].Line != diags[j].Line {
			return diags[i].Line < diags[j].Line
		}
		if diags[i].Code != diags[j].Code {
			return diags[i].Code < diags[j].Code
		}
		return diags[i].Message < diags[j].Message
	})

	return diags, nil
}

func validateProjectIndex(root string) []intvalidate.Diagnostic {
	indexPath := filepath.Join(root, "project-index.yaml")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil
	}

	idx := parseProjectIndexPaths(string(data))
	var diags []intvalidate.Diagnostic
	for _, req := range idx.Requirements {
		for _, rel := range req.Code {
			if !pathExists(root, rel) {
				diags = append(diags, intvalidate.Diagnostic{
					Path:     indexPath,
					Line:     1,
					Severity: intvalidate.SeverityError,
					Code:     CodeMissingCodePath,
					Message:  fmt.Sprintf("requirement %s references missing code path %q", req.ID, rel),
				})
			}
		}
		for _, rel := range req.Tests {
			if !pathExists(root, rel) {
				diags = append(diags, intvalidate.Diagnostic{
					Path:     indexPath,
					Line:     1,
					Severity: intvalidate.SeverityError,
					Code:     CodeMissingTestPath,
					Message:  fmt.Sprintf("requirement %s references missing test path %q", req.ID, rel),
				})
			}
		}
		for _, rel := range req.RelatedDocs {
			if !pathExists(root, rel) {
				diags = append(diags, intvalidate.Diagnostic{
					Path:     indexPath,
					Line:     1,
					Severity: intvalidate.SeverityWarning,
					Code:     CodeMissingRelatedDocPath,
					Message:  fmt.Sprintf("requirement %s references missing related_docs path %q", req.ID, rel),
				})
			}
		}
	}
	return diags
}

func validateSkillReferencePaths(root string) []intvalidate.Diagnostic {
	var diags []intvalidate.Diagnostic
	for _, skillPath := range skillFiles(root) {
		data, err := os.ReadFile(skillPath)
		if err != nil {
			continue
		}
		_, body, _, _ := splitFrontmatter(string(data))
		for idx, line := range strings.Split(body, "\n") {
			for _, match := range skillReferenceRE.FindAllString(line, -1) {
				if pathExists(filepath.Dir(skillPath), match) {
					continue
				}
				diags = append(diags, intvalidate.Diagnostic{
					Path:     skillPath,
					Line:     idx + 1,
					Severity: intvalidate.SeverityWarning,
					Code:     CodeMissingSkillReference,
					Message:  fmt.Sprintf("skill references missing file %q", match),
				})
			}
		}
	}
	return diags
}

func validatePluginSync(root string, opts Options) ([]intvalidate.Diagnostic, error) {
	cmdArgs := opts.SyncCheckCommand
	if len(cmdArgs) == 0 {
		cmdArgs = []string{"python3", "scripts/sync_codex_plugin.py", "--check"}
	}
	if len(cmdArgs) == 0 {
		return nil, nil
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = "scripts/sync_codex_plugin.py --check detected generated plugin drift"
		}
		return []intvalidate.Diagnostic{{
			Path:     filepath.Join(root, "scripts", "sync_codex_plugin.py"),
			Line:     1,
			Severity: intvalidate.SeverityError,
			Code:     CodePluginSyncDrift,
			Message:  message,
		}}, nil
	}
	return nil, fmt.Errorf("validate-docs: run sync check: %w", err)
}

func validateAgentSiblingParity(root string) []intvalidate.Diagnostic {
	leftDir := filepath.Join(root, "cmd", "initdata", "agents")
	rightDir := filepath.Join(root, ".github", "workbuddy", "agents")
	leftEntries, err1 := os.ReadDir(leftDir)
	rightEntries, err2 := os.ReadDir(rightDir)
	if err1 != nil || err2 != nil {
		return nil
	}

	left := make(map[string]frontmatterSignature)
	for _, entry := range leftEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		sig, err := loadFrontmatterSignature(filepath.Join(leftDir, entry.Name()))
		if err != nil {
			continue
		}
		left[entry.Name()] = sig
	}

	var diags []intvalidate.Diagnostic
	for _, entry := range rightEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		rightPath := filepath.Join(rightDir, entry.Name())
		rightSig, err := loadFrontmatterSignature(rightPath)
		if err != nil {
			continue
		}
		leftSig, ok := left[entry.Name()]
		if !ok {
			continue
		}
		if leftSig.String() == rightSig.String() {
			continue
		}
		diags = append(diags, intvalidate.Diagnostic{
			Path:     rightPath,
			Line:     1,
			Severity: intvalidate.SeverityError,
			Code:     CodeAgentFrontmatterMismatch,
			Message:  fmt.Sprintf("agent %s frontmatter shape differs: cmd/initdata=%s vs .github/workbuddy=%s", entry.Name(), leftSig.String(), rightSig.String()),
		})
	}
	return diags
}

func validateSkillCopyParity(root string) []intvalidate.Diagnostic {
	codexDir := filepath.Join(root, ".codex", "skills")
	claudeDir := filepath.Join(root, ".claude", "plugins", "workbuddy", "skills")
	codexEntries, err1 := os.ReadDir(codexDir)
	claudeEntries, err2 := os.ReadDir(claudeDir)
	if err1 != nil || err2 != nil {
		return nil
	}

	claudeSkills := map[string]string{}
	for _, entry := range claudeEntries {
		if !entry.IsDir() {
			continue
		}
		claudeSkills[entry.Name()] = filepath.Join(claudeDir, entry.Name())
	}

	var diags []intvalidate.Diagnostic
	for _, entry := range codexEntries {
		if !entry.IsDir() {
			continue
		}
		claudePath, ok := claudeSkills[entry.Name()]
		if !ok {
			continue
		}
		codexPath := filepath.Join(codexDir, entry.Name())
		codexSkill := filepath.Join(codexPath, "SKILL.md")
		claudeSkill := filepath.Join(claudePath, "SKILL.md")
		left, err := os.ReadFile(codexSkill)
		if err != nil {
			continue
		}
		right, err := os.ReadFile(claudeSkill)
		if err != nil {
			continue
		}
		if bytes.Equal(left, right) {
			continue
		}
		if pathExists(codexPath, "WHY-TWO-COPIES.md") || pathExists(claudePath, "WHY-TWO-COPIES.md") {
			continue
		}
		diags = append(diags, intvalidate.Diagnostic{
			Path:     codexSkill,
			Line:     1,
			Severity: intvalidate.SeverityWarning,
			Code:     CodeDuplicateSkillDivergence,
			Message:  fmt.Sprintf("skill %q differs across .codex and .claude copies; add WHY-TWO-COPIES.md or deduplicate", entry.Name()),
		})
	}
	return diags
}

func validateSkillHygiene(root string) []intvalidate.Diagnostic {
	var diags []intvalidate.Diagnostic
	for _, skillPath := range skillFiles(root) {
		data, err := os.ReadFile(skillPath)
		if err != nil {
			continue
		}
		header, body, bodyStartLine, hasFrontmatter := splitFrontmatter(string(data))
		if !hasFrontmatter {
			continue
		}
		meta, err := parseStringMap(header)
		if err != nil {
			continue
		}
		dirName := filepath.Base(filepath.Dir(skillPath))
		if name := strings.TrimSpace(meta["name"]); name != dirName {
			diags = append(diags, intvalidate.Diagnostic{
				Path:     skillPath,
				Line:     1,
				Severity: intvalidate.SeverityError,
				Code:     CodeSkillNameMismatch,
				Message:  fmt.Sprintf("skill frontmatter name %q does not match directory %q", name, dirName),
			})
		}
		if description := strings.TrimSpace(meta["description"]); description != "" && len([]rune(description)) < 50 {
			diags = append(diags, intvalidate.Diagnostic{
				Path:     skillPath,
				Line:     1,
				Severity: intvalidate.SeverityWarning,
				Code:     CodeSkillDescriptionTooShort,
				Message:  fmt.Sprintf("skill description is too short (%d chars); make it specific enough to trigger reliably", len([]rune(description))),
			})
		}
		for _, diag := range scanSkillFlagEnumerations(skillPath, body, bodyStartLine) {
			diags = append(diags, diag)
		}
	}
	return diags
}

func scanSkillFlagEnumerations(skillPath, body string, bodyStartLine int) []intvalidate.Diagnostic {
	lines := strings.Split(body, "\n")
	inFence := false
	streak := 0
	startLine := 0
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			streak = 0
			continue
		}
		if inFence {
			continue
		}
		if skillFlagLineRE.MatchString(line) {
			if streak == 0 {
				startLine = bodyStartLine + idx
			}
			streak++
			continue
		}
		if streak >= 3 {
			return []intvalidate.Diagnostic{{
				Path:     skillPath,
				Line:     startLine,
				Severity: intvalidate.SeverityWarning,
				Code:     CodeSkillFlagEnumeration,
				Message:  "skill body appears to enumerate CLI flags directly; prefer pointing readers at `<cmd> --help`",
			}}
		}
		streak = 0
	}
	if streak >= 3 {
		return []intvalidate.Diagnostic{{
			Path:     skillPath,
			Line:     startLine,
			Severity: intvalidate.SeverityWarning,
			Code:     CodeSkillFlagEnumeration,
			Message:  "skill body appears to enumerate CLI flags directly; prefer pointing readers at `<cmd> --help`",
		}}
	}
	return nil
}

type frontmatterSignature struct {
	NameKind      string
	RoleKind      string
	RuntimeKind   string
	ContextKind   string
	TriggersKind  string
	TriggerStates []string
}

func (s frontmatterSignature) String() string {
	parts := []string{
		"name=" + s.NameKind,
		"role=" + s.RoleKind,
		"runtime=" + s.RuntimeKind,
		"context=" + s.ContextKind,
		"triggers=" + s.TriggersKind,
	}
	if len(s.TriggerStates) > 0 {
		parts = append(parts, "trigger.state="+strings.Join(s.TriggerStates, ","))
	}
	return strings.Join(parts, ";")
}

func loadFrontmatterSignature(path string) (frontmatterSignature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return frontmatterSignature{}, err
	}
	header, _, _, ok := splitFrontmatter(string(data))
	if !ok {
		return frontmatterSignature{}, fmt.Errorf("missing frontmatter")
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(header), &node); err != nil {
		return frontmatterSignature{}, err
	}
	if len(node.Content) == 0 {
		return frontmatterSignature{}, fmt.Errorf("empty frontmatter")
	}
	mapping := node.Content[0]
	sig := frontmatterSignature{
		NameKind:    yamlKind(mappingValue(mapping, "name")),
		RoleKind:    yamlKind(mappingValue(mapping, "role")),
		RuntimeKind: yamlKind(mappingValue(mapping, "runtime")),
		ContextKind: yamlKind(mappingValue(mapping, "context")),
		TriggersKind: yamlKind(mappingValue(mapping,
			"triggers")),
	}
	triggers := mappingValue(mapping, "triggers")
	if triggers != nil && triggers.Kind == yaml.SequenceNode {
		for _, item := range triggers.Content {
			sig.TriggerStates = append(sig.TriggerStates, yamlKind(mappingValue(item, "state")))
		}
	}
	return sig, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
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

func yamlKind(node *yaml.Node) string {
	if node == nil {
		return "missing"
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	default:
		return fmt.Sprintf("kind-%d", node.Kind)
	}
}

func parseStringMap(header string) (map[string]string, error) {
	var meta map[string]any
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(meta))
	for key, value := range meta {
		result[key] = strings.TrimSpace(fmt.Sprint(value))
	}
	return result, nil
}

func parseProjectIndexPaths(text string) projectIndex {
	var idx projectIndex
	var current *projectRequirement
	currentField := ""

	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(line, "  - id:") {
			req := projectRequirement{ID: strings.TrimSpace(strings.TrimPrefix(line, "  - id:"))}
			idx.Requirements = append(idx.Requirements, req)
			current = &idx.Requirements[len(idx.Requirements)-1]
			currentField = ""
			continue
		}
		if current == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "    code:"):
			currentField = "code"
			appendInlineProjectIndexList(current, currentField, strings.TrimSpace(strings.TrimPrefix(line, "    code:")))
		case strings.HasPrefix(line, "    tests:"):
			currentField = "tests"
			appendInlineProjectIndexList(current, currentField, strings.TrimSpace(strings.TrimPrefix(line, "    tests:")))
		case strings.HasPrefix(line, "    related_docs:"):
			currentField = "related_docs"
			appendInlineProjectIndexList(current, currentField, strings.TrimSpace(strings.TrimPrefix(line, "    related_docs:")))
		case strings.HasPrefix(line, "    deps:"):
			currentField = ""
			current = nil
		case strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      - "):
			currentField = ""
		case strings.HasPrefix(line, "      - ") && currentField != "":
			item := strings.TrimSpace(strings.TrimPrefix(line, "      - "))
			appendProjectIndexField(current, currentField, item)
		case trimmed == "":
			continue
		}
	}

	return idx
}

func appendInlineProjectIndexList(req *projectRequirement, field, value string) {
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return
	}
	for _, item := range strings.Split(strings.Trim(value, "[]"), ",") {
		item = strings.TrimSpace(strings.Trim(item, `"'`))
		if item == "" {
			continue
		}
		appendProjectIndexField(req, field, item)
	}
}

func appendProjectIndexField(req *projectRequirement, field, value string) {
	switch field {
	case "code":
		req.Code = append(req.Code, value)
	case "tests":
		req.Tests = append(req.Tests, value)
	case "related_docs":
		req.RelatedDocs = append(req.RelatedDocs, value)
	}
}

func splitFrontmatter(text string) (header, body string, bodyStartLine int, ok bool) {
	if !strings.HasPrefix(text, "---\n") {
		return "", text, 1, false
	}
	parts := strings.SplitN(text, "\n---\n", 2)
	if len(parts) != 2 {
		return "", text, 1, false
	}
	header = strings.TrimPrefix(parts[0], "---\n")
	body = parts[1]
	bodyStartLine = strings.Count(parts[0], "\n") + 2
	return header, body, bodyStartLine, true
}

func skillFiles(root string) []string {
	patterns := []string{
		filepath.Join(root, ".codex", "skills", "*", "SKILL.md"),
		filepath.Join(root, ".claude", "plugins", "workbuddy", "skills", "*", "SKILL.md"),
		filepath.Join(root, "plugins", "workbuddy", "skills", "*", "SKILL.md"),
	}
	seen := make(map[string]struct{})
	var files []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			files = append(files, match)
		}
	}
	sort.Strings(files)
	return files
}

func pathExists(base, rel string) bool {
	_, err := os.Stat(filepath.Join(base, rel))
	return err == nil
}
