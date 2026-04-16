package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	"gopkg.in/yaml.v3"
)

var yamlLineErrorRe = regexp.MustCompile(`line (\d+):`)

// Diagnostic represents a single validation failure.
type Diagnostic struct {
	Path    string
	Line    int
	Message string
}

func (d Diagnostic) String() string {
	line := d.Line
	if line <= 0 {
		line = 1
	}
	return fmt.Sprintf("%s:%d: %s", filepath.Base(d.Path), line, d.Message)
}

type agentDoc struct {
	Name         string
	Path         string
	NameLine     int
	TriggerLines []labelRef
}

type labelRef struct {
	Label string
	Line  int
}

type workflowDoc struct {
	Name       string
	Path       string
	NameLine   int
	StateOrder []string
	States     map[string]*stateDoc
}

type stateDoc struct {
	Name           string
	Line           int
	EnterLabel     string
	EnterLabelLine int
	Agent          string
	AgentLine      int
	Transitions    []transitionDoc
}

type transitionDoc struct {
	To     string
	ToLine int
}

// ValidateDir validates a .github/workbuddy configuration directory.
func ValidateDir(configDir string) ([]Diagnostic, error) {
	info, err := os.Stat(configDir)
	if err != nil {
		return nil, fmt.Errorf("validate: config directory %q: %w", configDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("validate: %q is not a directory", configDir)
	}

	var diags []Diagnostic
	diags = append(diags, validateConfigFile(filepath.Join(configDir, "config.yaml"))...)

	agents, agentDiags, err := loadAgents(filepath.Join(configDir, "agents"))
	if err != nil {
		return nil, err
	}
	diags = append(diags, agentDiags...)

	workflows, workflowDiags, err := loadWorkflows(filepath.Join(configDir, "workflows"))
	if err != nil {
		return nil, err
	}
	diags = append(diags, workflowDiags...)

	enterLabels := make(map[string]struct{})
	for _, wf := range workflows {
		for _, stateName := range wf.StateOrder {
			label := strings.TrimSpace(wf.States[stateName].EnterLabel)
			if label != "" {
				enterLabels[label] = struct{}{}
			}
		}
	}

	for _, wf := range workflows {
		diags = append(diags, validateWorkflowGraph(wf)...)
		for _, stateName := range wf.StateOrder {
			state := wf.States[stateName]
			if strings.TrimSpace(state.Agent) == "" {
				continue
			}
			if _, ok := agents[state.Agent]; !ok {
				diags = append(diags, Diagnostic{
					Path:    wf.Path,
					Line:    state.AgentLine,
					Message: fmt.Sprintf("workflow %q references unknown agent %q", wf.Name, state.Agent),
				})
			}
		}
	}

	agentNames := make([]string, 0, len(agents))
	for name := range agents {
		agentNames = append(agentNames, name)
	}
	sort.Strings(agentNames)
	for _, name := range agentNames {
		agent := agents[name]
		for _, trigger := range agent.TriggerLines {
			if strings.TrimSpace(trigger.Label) == "" {
				continue
			}
			if _, ok := enterLabels[trigger.Label]; !ok {
				diags = append(diags, Diagnostic{
					Path:    agent.Path,
					Line:    trigger.Line,
					Message: fmt.Sprintf("agent %q trigger label %q does not match any workflow state label", agent.Name, trigger.Label),
				})
			}
		}
	}

	sort.Slice(diags, func(i, j int) bool {
		a := diags[i]
		b := diags[j]
		ab := filepath.Base(a.Path)
		bb := filepath.Base(b.Path)
		if ab != bb {
			return ab < bb
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Message < b.Message
	})

	return diags, nil
}

func validateConfigFile(path string) []Diagnostic {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Diagnostic{{Path: path, Line: 1, Message: "missing config.yaml"}}
		}
		return []Diagnostic{{Path: path, Line: 1, Message: err.Error()}}
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return []Diagnostic{yamlDiagnostic(path, 1, "invalid config.yaml", err)}
	}
	return nil
}

func loadAgents(dir string) (map[string]*agentDoc, []Diagnostic, error) {
	agents := make(map[string]*agentDoc)
	var diags []Diagnostic

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return agents, diags, nil
		}
		return nil, nil, fmt.Errorf("validate: read agents dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		agent, parseDiags := parseAgentFile(path)
		diags = append(diags, parseDiags...)
		if agent != nil && strings.TrimSpace(agent.Name) != "" {
			agents[agent.Name] = agent
		}
	}

	return agents, diags, nil
}

func loadWorkflows(dir string) ([]*workflowDoc, []Diagnostic, error) {
	var workflows []*workflowDoc
	var diags []Diagnostic

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return workflows, diags, nil
		}
		return nil, nil, fmt.Errorf("validate: read workflows dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		workflow, parseDiags := parseWorkflowFile(path)
		diags = append(diags, parseDiags...)
		if workflow != nil {
			workflows = append(workflows, workflow)
		}
	}

	sort.Slice(workflows, func(i, j int) bool {
		return filepath.Base(workflows[i].Path) < filepath.Base(workflows[j].Path)
	})
	return workflows, diags, nil
}

func parseAgentFile(path string) (*agentDoc, []Diagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []Diagnostic{{Path: path, Line: 1, Message: err.Error()}}
	}

	fm, _, fmStartLine, _, splitErr := splitFrontmatter(string(data))
	if splitErr != nil {
		return nil, []Diagnostic{{Path: path, Line: 1, Message: splitErr.Error()}}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return nil, []Diagnostic{yamlDiagnostic(path, fmStartLine, "invalid agent frontmatter", err)}
	}
	if len(doc.Content) == 0 {
		return nil, []Diagnostic{{Path: path, Line: fmStartLine, Message: "empty agent frontmatter"}}
	}

	root := doc.Content[0]
	agent := &agentDoc{Path: path}
	agent.Name, agent.NameLine = scalarValue(root, "name", fmStartLine)

	triggersNode, _ := mappingValue(root, "triggers")
	if triggersNode != nil && triggersNode.Kind == yaml.SequenceNode {
		for _, item := range triggersNode.Content {
			label, line := scalarValue(item, "label", fmStartLine)
			if strings.TrimSpace(label) != "" {
				agent.TriggerLines = append(agent.TriggerLines, labelRef{Label: label, Line: line})
			}
		}
	}

	var diags []Diagnostic
	if strings.TrimSpace(agent.Name) == "" {
		diags = append(diags, Diagnostic{Path: path, Line: fmStartLine, Message: "missing agent name"})
	}
	return agent, diags
}

func parseWorkflowFile(path string) (*workflowDoc, []Diagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []Diagnostic{{Path: path, Line: 1, Message: err.Error()}}
	}

	fm, body, fmStartLine, bodyStartLine, splitErr := splitFrontmatter(string(data))
	if splitErr != nil {
		return nil, []Diagnostic{{Path: path, Line: 1, Message: splitErr.Error()}}
	}

	var frontmatter yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &frontmatter); err != nil {
		return nil, []Diagnostic{yamlDiagnostic(path, fmStartLine, "invalid workflow frontmatter", err)}
	}
	if len(frontmatter.Content) == 0 {
		return nil, []Diagnostic{{Path: path, Line: fmStartLine, Message: "empty workflow frontmatter"}}
	}

	yamlBlock, yamlStartLine, ok := firstYAMLCodeBlock(body, bodyStartLine)
	if !ok {
		return nil, []Diagnostic{{Path: path, Line: bodyStartLine, Message: "missing workflow states yaml block"}}
	}

	var stateTree yaml.Node
	if err := yaml.Unmarshal([]byte(yamlBlock), &stateTree); err != nil {
		return nil, []Diagnostic{yamlDiagnostic(path, yamlStartLine, "invalid workflow states yaml", err)}
	}
	if len(stateTree.Content) == 0 {
		return nil, []Diagnostic{{Path: path, Line: yamlStartLine, Message: "empty workflow states yaml"}}
	}

	root := frontmatter.Content[0]
	workflow := &workflowDoc{
		Path:   path,
		States: make(map[string]*stateDoc),
	}
	workflow.Name, workflow.NameLine = scalarValue(root, "name", fmStartLine)

	statesWrapper, _ := mappingValue(stateTree.Content[0], "states")
	if statesWrapper == nil || statesWrapper.Kind != yaml.MappingNode {
		return nil, []Diagnostic{{Path: path, Line: yamlStartLine, Message: "workflow states must be a mapping"}}
	}

	for i := 0; i+1 < len(statesWrapper.Content); i += 2 {
		keyNode := statesWrapper.Content[i]
		valueNode := statesWrapper.Content[i+1]
		name := strings.TrimSpace(keyNode.Value)
		state := &stateDoc{
			Name: name,
			Line: absoluteLine(yamlStartLine, keyNode.Line),
		}
		state.EnterLabel, state.EnterLabelLine = scalarValue(valueNode, "enter_label", yamlStartLine)
		state.Agent, state.AgentLine = scalarValue(valueNode, "agent", yamlStartLine)

		transitionsNode, _ := mappingValue(valueNode, "transitions")
		if transitionsNode != nil && transitionsNode.Kind == yaml.SequenceNode {
			for _, item := range transitionsNode.Content {
				toValue, toLine := scalarValue(item, "to", yamlStartLine)
				state.Transitions = append(state.Transitions, transitionDoc{
					To:     toValue,
					ToLine: toLine,
				})
			}
		}

		workflow.StateOrder = append(workflow.StateOrder, name)
		workflow.States[name] = state
	}

	var diags []Diagnostic
	if strings.TrimSpace(workflow.Name) == "" {
		diags = append(diags, Diagnostic{Path: path, Line: fmStartLine, Message: "missing workflow name"})
	}
	return workflow, diags
}

func validateWorkflowGraph(wf *workflowDoc) []Diagnostic {
	var diags []Diagnostic
	if len(wf.StateOrder) == 0 {
		return []Diagnostic{{
			Path:    wf.Path,
			Line:    wf.NameLine,
			Message: fmt.Sprintf("workflow %q defines no states", wf.Name),
		}}
	}

	index := make(map[string]int, len(wf.StateOrder))
	for i, stateName := range wf.StateOrder {
		index[stateName] = i
	}

	seenEdges := make(map[string]transitionDoc)
	hasFallbackEdge := false
	for _, fromState := range wf.StateOrder {
		state := wf.States[fromState]
		for _, transition := range state.Transitions {
			key := fromState + "\x00" + transition.To
			if prev, ok := seenEdges[key]; ok {
				line := transition.ToLine
				if line <= 0 {
					line = state.Line
				}
				diags = append(diags, Diagnostic{
					Path:    wf.Path,
					Line:    line,
					Message: fmt.Sprintf("workflow %q has duplicate edge %q -> %q (first defined at line %d)", wf.Name, fromState, transition.To, prev.ToLine),
				})
			} else {
				seenEdges[key] = transition
			}

			if toIndex, ok := index[transition.To]; ok && toIndex < index[fromState] {
				hasFallbackEdge = true
			}
		}
	}

	entry := wf.StateOrder[0]
	visited := make(map[string]bool, len(wf.StateOrder))
	var walk func(string)
	walk = func(stateName string) {
		if visited[stateName] {
			return
		}
		visited[stateName] = true
		state := wf.States[stateName]
		for _, transition := range state.Transitions {
			if _, ok := wf.States[transition.To]; ok {
				walk(transition.To)
			}
		}
	}
	walk(entry)

	for _, stateName := range wf.StateOrder {
		if stateName == config.StateNameFailed {
			continue
		}
		if visited[stateName] {
			continue
		}
		state := wf.States[stateName]
		diags = append(diags, Diagnostic{
			Path:    wf.Path,
			Line:    state.Line,
			Message: fmt.Sprintf("workflow %q has unreachable state %q from entry state %q", wf.Name, stateName, entry),
		})
	}

	if hasFallbackEdge {
		failed, ok := wf.States[config.StateNameFailed]
		if !ok || len(failed.Transitions) != 0 {
			line := wf.NameLine
			if ok && failed.Line > 0 {
				line = failed.Line
			}
			diags = append(diags, Diagnostic{
				Path:    wf.Path,
				Line:    line,
				Message: fmt.Sprintf("workflow %q contains fallback edges and requires a terminal %q state", wf.Name, config.StateNameFailed),
			})
		}
	}

	return diags
}

func splitFrontmatter(content string) (frontmatter string, body string, fmStartLine int, bodyStartLine int, err error) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", 0, 0, fmt.Errorf("missing YAML frontmatter delimiter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", 0, 0, fmt.Errorf("missing closing YAML frontmatter delimiter")
	}
	return strings.Join(lines[1:end], "\n"), strings.Join(lines[end+1:], "\n"), 2, end + 2, nil
}

func firstYAMLCodeBlock(body string, bodyStartLine int) (string, int, bool) {
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```yaml" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				return strings.Join(lines[i+1:j], "\n"), bodyStartLine + i + 1, true
			}
		}
		return "", bodyStartLine + i, false
	}
	return "", 0, false
}

func scalarValue(node *yaml.Node, key string, baseLine int) (string, int) {
	value, _ := mappingValue(node, key)
	if value == nil {
		return "", 0
	}
	return strings.TrimSpace(value.Value), absoluteLine(baseLine, value.Line)
}

func mappingValue(node *yaml.Node, key string) (*yaml.Node, int) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, 0
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode.Value == key {
			return node.Content[i+1], keyNode.Line
		}
	}
	return nil, 0
}

func absoluteLine(baseLine, relativeLine int) int {
	if relativeLine <= 0 {
		return baseLine
	}
	return baseLine + relativeLine - 1
}

func yamlDiagnostic(path string, baseLine int, prefix string, err error) Diagnostic {
	line := baseLine
	if match := yamlLineErrorRe.FindStringSubmatch(err.Error()); len(match) == 2 {
		if parsed, convErr := strconv.Atoi(match[1]); convErr == nil {
			line = absoluteLine(baseLine, parsed)
		}
	}
	return Diagnostic{
		Path:    path,
		Line:    line,
		Message: fmt.Sprintf("%s: %v", prefix, err),
	}
}
