package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDevAgentTemplate_NoRolloutPreservesLegacyBranchAndPRText(t *testing.T) {
	rendered := renderDevAgentTemplate(t, &TaskContext{
		Repo: "octo/example",
		Issue: IssueContext{
			Number: 42,
			Title:  "Test issue",
			Body:   "Body",
		},
	})

	if !strings.Contains(rendered, "You are working on branch `workbuddy/issue-42`.") {
		t.Fatalf("rendered prompt missing legacy branch text:\n%s", rendered)
	}
	if strings.Contains(rendered, "/rollout-") {
		t.Fatalf("non-rollout prompt should not mention rollout branch suffix:\n%s", rendered)
	}
	if strings.Contains(rendered, "[rollout ") {
		t.Fatalf("non-rollout prompt should not mention rollout PR suffix:\n%s", rendered)
	}
}

func renderDevAgentTemplate(t *testing.T, task *TaskContext) string {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	templatePath := filepath.Join(filepath.Dir(currentFile), "..", "..", ".github", "workbuddy", "agents", "dev-agent.md")
	data, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read dev-agent template: %v", err)
	}
	parts := strings.SplitN(string(data), "---\n", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected dev-agent template frontmatter layout")
	}
	rendered, err := RenderCommandRaw(strings.TrimSpace(parts[2]), task)
	if err != nil {
		t.Fatalf("RenderCommandRaw: %v", err)
	}
	return rendered
}
