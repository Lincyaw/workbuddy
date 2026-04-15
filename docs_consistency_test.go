package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type docsProjectIndex struct {
	Documentation struct {
		Documents []struct {
			Path        string   `yaml:"path"`
			Notes       string   `yaml:"notes"`
			RelatedCode []string `yaml:"related_code"`
		} `yaml:"documents"`
	} `yaml:"documentation"`
	Requirements []struct {
		ID                 string   `yaml:"id"`
		Description        string   `yaml:"description"`
		AcceptanceCriteria []string `yaml:"acceptance_criteria"`
		Notes              string   `yaml:"notes"`
		RelatedDocs        []string `yaml:"related_docs"`
	} `yaml:"requirements"`
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func loadProjectIndex(t *testing.T) docsProjectIndex {
	t.Helper()

	var idx docsProjectIndex
	if err := yaml.Unmarshal([]byte(readRepoFile(t, "project-index.yaml")), &idx); err != nil {
		t.Fatalf("parse project-index.yaml: %v", err)
	}
	return idx
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertContainsAll(t *testing.T, path, content string, want []string) {
	t.Helper()

	for _, needle := range want {
		if !strings.Contains(content, needle) {
			t.Fatalf("%s missing %q", path, needle)
		}
	}
}

func TestRetryFailureDocsMigratedToImplemented(t *testing.T) {
	if _, err := os.Stat("docs/mismatch/retry-and-failure-drift.md"); !os.IsNotExist(err) {
		t.Fatalf("docs/mismatch/retry-and-failure-drift.md should be removed, stat err=%v", err)
	}

	architecture := readRepoFile(t, "docs/implemented/current-architecture.md")
	assertContainsAll(t, "docs/implemented/current-architecture.md", architecture, []string{
		"## 当前重试与失败边界",
		"TypeCycleLimitReached",
		"TypeTransitionToFailed",
		"status:failed",
		"needs-human",
		"ResetDedup()",
		"MarkAgentCompleted",
		"CheckStuck",
		"internal/statemachine/statemachine.go",
		"internal/store/store.go",
	})

	for _, workflowPath := range []string{
		".github/workbuddy/workflows/feature-dev.md",
		".github/workbuddy/workflows/bugfix.md",
	} {
		workflow := readRepoFile(t, workflowPath)
		if strings.Contains(workflow, `action: add_label "needs-human"`) {
			t.Fatalf("%s should not claim automatic needs-human label writeback", workflowPath)
		}
		assertContainsAll(t, workflowPath, workflow, []string{
			"`failed` 仍然是 workflow schema 中可识别的终态 label",
			"Go runtime 不会在 retry 超限时直接写入",
			"status:failed",
			"needs-human",
			"record retry/failure intent",
		})
	}
}

func TestRetryFailureDocIndexesStaySynced(t *testing.T) {
	deletedPath := "docs/mismatch/retry-and-failure-drift.md"

	for _, path := range []string{
		"docs/index.md",
		"docs/mismatch/index.md",
	} {
		if strings.Contains(readRepoFile(t, path), deletedPath) {
			t.Fatalf("%s still references %s", path, deletedPath)
		}
	}

	idx := loadProjectIndex(t)

	foundArchitectureDoc := false
	foundREQ003 := false
	for _, doc := range idx.Documentation.Documents {
		if doc.Path == deletedPath {
			t.Fatalf("project-index.yaml still lists %s", deletedPath)
		}
		if doc.Path == "docs/implemented/current-architecture.md" {
			foundArchitectureDoc = true
			if !containsString(doc.RelatedCode, "internal/store/store.go") {
				t.Fatal("current-architecture project-index entry should include internal/store/store.go")
			}
			if !strings.Contains(doc.Notes, "retry/failure") {
				t.Fatal("current-architecture project-index notes should mention the retry/failure boundary")
			}
		}
	}

	for _, req := range idx.Requirements {
		if req.ID != "REQ-003" {
			continue
		}
		foundREQ003 = true
		if !containsString(req.RelatedDocs, "docs/implemented/current-architecture.md") {
			t.Fatal("REQ-003 should point at docs/implemented/current-architecture.md")
		}
		if !strings.Contains(req.Notes, "TypeCycleLimitReached") || !strings.Contains(req.Notes, "failed/needs-human") {
			t.Fatal("REQ-003 notes should describe the implemented retry/failure boundary")
		}
		assertContainsAll(t, "project-index.yaml REQ-003", req.Description, []string{
			"record retry/failure intent",
		})
		if containsString(req.AcceptanceCriteria, "AC-003-6: CycleTracker 计数 >= max_retries 时，拒绝回退，转到 failed 状态并添加 needs-human label") {
			t.Fatal("REQ-003 should not claim automatic failed/needs-human writeback on retry overflow")
		}
		matchedAC0036 := false
		for _, criterion := range req.AcceptanceCriteria {
			if strings.Contains(criterion, "AC-003-6:") {
				matchedAC0036 = true
				assertContainsAll(t, "project-index.yaml REQ-003 AC-003-6", criterion, []string{
					"TypeCycleLimitReached",
					"TypeTransitionToFailed",
					"agent 或人工执行",
				})
			}
		}
		if !matchedAC0036 {
			t.Fatal("REQ-003 should include AC-003-6")
		}
	}

	if !foundArchitectureDoc {
		t.Fatal("project-index.yaml is missing docs/implemented/current-architecture.md")
	}
	if !foundREQ003 {
		t.Fatal("project-index.yaml is missing REQ-003")
	}
}

func TestIssueDependenciesPlannedDocIndexed(t *testing.T) {
	docPath := "docs/planned/issue-dependencies.md"
	docContent := readRepoFile(t, docPath)
	assertContainsAll(t, docPath, docContent, []string{
		"# Issue Dependency Mechanism",
		"## Goal",
		"## 当前状态",
		"## Convention Compliance",
		"## 目标状态",
		"## Concrete Code Touch Points",
		"## Rejected Alternatives",
		"## Distance From Current Code",
		"## Migration from Prior Draft",
		"## Migration Path",
		"Option A",
		"dependency-resolver-agent",
		"不是字面量 dotted key",
		"`status:blocked`",
		"`override:force-unblock`",
	})

	for _, path := range []string{
		"docs/index.md",
		"docs/planned/index.md",
	} {
		if !strings.Contains(readRepoFile(t, path), docPath) {
			t.Fatalf("%s missing %s", path, docPath)
		}
	}

	idx := loadProjectIndex(t)
	found := false
	for _, docEntry := range idx.Documentation.Documents {
		if docEntry.Path != docPath {
			continue
		}
		found = true
		if !containsString(docEntry.RelatedCode, "internal/poller/poller.go") {
			t.Fatal("issue-dependencies project-index entry should include internal/poller/poller.go")
		}
		if !containsString(docEntry.RelatedCode, "internal/statemachine/statemachine.go") {
			t.Fatal("issue-dependencies project-index entry should include internal/statemachine/statemachine.go")
		}
		if !strings.Contains(docEntry.Notes, "cycle detection") {
			t.Fatal("issue-dependencies project-index notes should mention cycle detection")
		}
	}

	if !found {
		t.Fatalf("project-index.yaml is missing %s", docPath)
	}
}

func TestAgentCatalogMigratedToImplemented(t *testing.T) {
	oldPath := "docs/planned/agent-catalog.md"
	newPath := "docs/implemented/agent-catalog.md"

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("%s should be removed, stat err=%v", oldPath, err)
	}

	doc := readRepoFile(t, newPath)
	assertContainsAll(t, newPath, doc, []string{
		"# Agent Catalog",
		"状态：implemented",
		"triage-agent",
		"docs-agent",
		"security-audit-agent",
		"dependency-bump-agent",
		"release-agent",
		"output_contract",
	})

	for _, path := range []string{
		"docs/index.md",
		"docs/implemented/index.md",
		"docs/planned/index.md",
	} {
		content := readRepoFile(t, path)
		if strings.Contains(content, oldPath) {
			t.Fatalf("%s still references %s", path, oldPath)
		}
	}

	idx := loadProjectIndex(t)
	foundNew := false
	for _, doc := range idx.Documentation.Documents {
		if doc.Path == oldPath {
			t.Fatalf("project-index.yaml still lists %s", oldPath)
		}
		if doc.Path != newPath {
			continue
		}
		foundNew = true
		if !containsString(doc.RelatedCode, ".github/workbuddy/agents/triage-agent.md") {
			t.Fatal("agent-catalog project-index entry should include triage-agent.md")
		}
		if !containsString(doc.RelatedCode, ".github/workbuddy/agents/release-agent.md") {
			t.Fatal("agent-catalog project-index entry should include release-agent.md")
		}
		if !strings.Contains(doc.Notes, "materializes the catalog agents") {
			t.Fatal("agent-catalog project-index notes should describe the implemented catalog materialization")
		}
	}

	if !foundNew {
		t.Fatalf("project-index.yaml is missing %s", newPath)
	}
}
