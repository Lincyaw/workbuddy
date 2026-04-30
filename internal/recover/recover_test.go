package recover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPruneWorktrees_RemovesClosedRolloutWorktreesOnly(t *testing.T) {
	repoRoot := t.TempDir()
	commonRoot := t.TempDir()
	wtRoot := filepath.Join(commonRoot, worktreesDir)
	openWT := filepath.Join(wtRoot, "issue-1")
	closedWT := filepath.Join(wtRoot, "issue-2", "rollout-2")
	for _, dir := range []string{openWT, closedWT} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}

	binDir := t.TempDir()
	gitScript := `#!/usr/bin/env bash
set -euo pipefail
cmd="${1:-}"
shift || true
case "$cmd" in
  config)
    echo "git@github.com:Lincyaw/workbuddy.git"
    ;;
  for-each-ref)
    printf '%s\n' "workbuddy/issue-1" "workbuddy/issue-2/rollout-2"
    ;;
  worktree)
    sub="${1:-}"
    shift || true
    case "$sub" in
      list)
        cat <<EOF
worktree ` + openWT + `
HEAD 1111111
branch refs/heads/workbuddy/issue-1

worktree ` + closedWT + `
HEAD 2222222
branch refs/heads/workbuddy/issue-2/rollout-2
EOF
        ;;
      remove)
        if [[ "${1:-}" == "--force" ]]; then
          shift
        fi
        rm -rf "$1"
        ;;
      prune)
        ;;
      *)
        echo "unexpected git worktree subcommand: $sub" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    echo "unexpected git command: $cmd" >&2
    exit 1
    ;;
esac
`
	ghScript := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' '[{"headRefName":"workbuddy/issue-1"}]'
`
	for name, content := range map[string]string{"git": gitScript, "gh": ghScript} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+oldPath)

	var stdout strings.Builder
	if err := pruneWorktrees(context.Background(), repoRoot, commonRoot, Options{
		RepoRoot: repoRoot,
		Stdout:   &stdout,
		Stderr:   &stdout,
	}); err != nil {
		t.Fatalf("pruneWorktrees: %v", err)
	}

	if _, err := os.Stat(openWT); err != nil {
		t.Fatalf("expected open worktree to remain, stat err=%v", err)
	}
	if _, err := os.Stat(closedWT); !os.IsNotExist(err) {
		t.Fatalf("expected closed rollout worktree to be removed, stat err=%v", err)
	}
	if !strings.Contains(stdout.String(), "rollout-2") {
		t.Fatalf("expected output to mention rollout worktree, got %q", stdout.String())
	}
}
