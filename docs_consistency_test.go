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
		ID          string   `yaml:"id"`
		Notes       string   `yaml:"notes"`
		RelatedDocs []string `yaml:"related_docs"`
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

func TestRetryFailureDocsMigratedToImplemented(t *testing.T) {
	if _, err := os.Stat("docs/mismatch/retry-and-failure-drift.md"); !os.IsNotExist(err) {
		t.Fatalf("docs/mismatch/retry-and-failure-drift.md should be removed, stat err=%v", err)
	}

	architecture := readRepoFile(t, "docs/implemented/current-architecture.md")
	for _, want := range []string{
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
	} {
		if !strings.Contains(architecture, want) {
			t.Fatalf("docs/implemented/current-architecture.md missing %q", want)
		}
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
	}

	if !foundArchitectureDoc {
		t.Fatal("project-index.yaml is missing docs/implemented/current-architecture.md")
	}
	if !foundREQ003 {
		t.Fatal("project-index.yaml is missing REQ-003")
	}
}
