package cmd

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

//go:embed initdata/* initdata/agents/* initdata/workflows/*
var initTemplates embed.FS

type initOpts struct {
	repo   string
	force  bool
	format string
}

type initResult struct {
	Repo        string   `json:"repo"`
	Directories []string `json:"directories"`
	Files       []string `json:"files"`
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Scaffold .github/workbuddy/ config for a new repository",
	Long: `Write a minimal, working workbuddy configuration into .github/workbuddy/:
config.yaml (repo identity + poll settings), agents/dev-agent.md and
agents/review-agent.md (prompt templates), and workflows/default.md (the
label-driven state machine). The scaffold is generic — tune it to the repo
afterwards. Combine with label creation to fully onboard a repo.`,
	Example: `  # Infer repo from git origin
  cd /path/to/my/repo
  workbuddy init

  # Explicit repo name
  workbuddy init --repo owner/name

  # Overwrite existing scaffold files
  workbuddy init --force`,
	RunE: runInitCmd,
}

func init() {
	initCmd.Flags().String("repo", "", "Repository in OWNER/NAME form; defaults to the local git origin when available")
	initCmd.Flags().Bool("force", false, "Overwrite existing scaffold files")
	addOutputFormatFlag(initCmd)
	rootCmd.AddCommand(initCmd)
}

func runInitCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseInitFlags(cmd)
	if err != nil {
		return err
	}
	return runInitWithOpts(cmd.Context(), ".", opts, cmd.OutOrStdout())
}

func parseInitFlags(cmd *cobra.Command) (*initOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	force, _ := cmd.Flags().GetBool("force")
	format, err := resolveOutputFormat(cmd, "init")
	if err != nil {
		return nil, err
	}
	return &initOpts{repo: strings.TrimSpace(repo), force: force, format: format}, nil
}

func runInitWithOpts(ctx context.Context, root string, opts *initOpts, stdout io.Writer) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("init: resolve root: %w", err)
	}
	repo, err := resolveInitRepo(ctx, absRoot, opts.repo)
	if err != nil {
		return err
	}

	files, err := initFiles(repo)
	if err != nil {
		return err
	}
	if err := writeInitFiles(absRoot, files, opts.force); err != nil {
		return err
	}

	directories := initDirs()
	filePaths := make([]string, 0, len(files))
	for _, file := range files {
		filePaths = append(filePaths, filepath.ToSlash(file.path))
	}
	if isJSONOutput(opts.format) {
		return writeJSON(stdout, initResult{
			Repo:        repo,
			Directories: directories,
			Files:       filePaths,
		})
	}

	for _, dir := range directories {
		if _, err := fmt.Fprintf(stdout, "created %s\n", filepath.ToSlash(dir)); err != nil {
			return fmt.Errorf("init: write output: %w", err)
		}
	}
	for _, file := range files {
		if _, err := fmt.Fprintf(stdout, "wrote %s\n", filepath.ToSlash(file.path)); err != nil {
			return fmt.Errorf("init: write output: %w", err)
		}
	}
	return nil
}

type scaffoldFile struct {
	path    string
	mode    fs.FileMode
	content []byte
}

func initDirs() []string {
	return []string{
		".github/workbuddy/agents",
		".github/workbuddy/workflows",
		".workbuddy/logs",
		".workbuddy/sessions",
		".workbuddy/worktrees",
	}
}

func initFiles(repo string) ([]scaffoldFile, error) {
	configBuf := bytes.Buffer{}
	tmpl, err := template.New("config").Parse(initConfigTemplate)
	if err != nil {
		return nil, fmt.Errorf("init: parse config.yaml template: %w", err)
	}
	if err := tmpl.Execute(&configBuf, struct {
		Repo string
	}{Repo: repo}); err != nil {
		return nil, fmt.Errorf("init: render config.yaml: %w", err)
	}

	devAgent, err := fs.ReadFile(initTemplates, "initdata/agents/dev-agent.md")
	if err != nil {
		return nil, fmt.Errorf("init: load dev-agent template: %w", err)
	}
	reviewAgent, err := fs.ReadFile(initTemplates, "initdata/agents/review-agent.md")
	if err != nil {
		return nil, fmt.Errorf("init: load review-agent template: %w", err)
	}
	workflow, err := fs.ReadFile(initTemplates, "initdata/workflows/default.md")
	if err != nil {
		return nil, fmt.Errorf("init: load workflow template: %w", err)
	}
	runtimeGitignore, err := fs.ReadFile(initTemplates, "initdata/workbuddy.gitignore")
	if err != nil {
		return nil, fmt.Errorf("init: load runtime gitignore template: %w", err)
	}

	return []scaffoldFile{
		{path: ".github/workbuddy/config.yaml", mode: 0o644, content: configBuf.Bytes()},
		{path: ".github/workbuddy/agents/dev-agent.md", mode: 0o644, content: devAgent},
		{path: ".github/workbuddy/agents/review-agent.md", mode: 0o644, content: reviewAgent},
		{path: ".github/workbuddy/workflows/default.md", mode: 0o644, content: workflow},
		{path: ".workbuddy/.gitignore", mode: 0o644, content: runtimeGitignore},
	}, nil
}

func writeInitFiles(root string, files []scaffoldFile, force bool) error {
	for _, dir := range initDirs() {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return fmt.Errorf("init: create directory %q: %w", dir, err)
		}
	}

	for _, file := range files {
		target := filepath.Join(root, file.path)
		if !force {
			if _, err := os.Stat(target); err == nil {
				return fmt.Errorf("init: target %q already exists (use --force to overwrite)", file.path)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("init: stat %q: %w", file.path, err)
			}
		}
		if err := os.WriteFile(target, file.content, file.mode); err != nil {
			return fmt.Errorf("init: write %q: %w", file.path, err)
		}
	}
	return nil
}

var repoSlugRe = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)

func resolveInitRepo(ctx context.Context, root, explicit string) (string, error) {
	if explicit != "" {
		if !repoSlugRe.MatchString(explicit) {
			return "", fmt.Errorf("init: --repo must be in OWNER/NAME form")
		}
		return explicit, nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", root, "config", "--get", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("init: could not infer repo from git origin; pass --repo OWNER/NAME")
	}
	repo := parseGitRemoteURL(strings.TrimSpace(string(out)))
	if repo == "" {
		return "", fmt.Errorf("init: could not parse git origin %q; pass --repo OWNER/NAME", strings.TrimSpace(string(out)))
	}
	return repo, nil
}

func parseGitRemoteURL(remote string) string {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, ".git")
	switch {
	case strings.HasPrefix(remote, "git@"):
		if idx := strings.Index(remote, ":"); idx >= 0 && idx+1 < len(remote) {
			candidate := remote[idx+1:]
			if repoSlugRe.MatchString(candidate) {
				return candidate
			}
		}
	case strings.HasPrefix(remote, "ssh://"), strings.HasPrefix(remote, "https://"), strings.HasPrefix(remote, "http://"):
		if idx := strings.Index(remote, "://"); idx >= 0 {
			path := remote[idx+3:]
			if slash := strings.Index(path, "/"); slash >= 0 && slash+1 < len(path) {
				candidate := path[slash+1:]
				if repoSlugRe.MatchString(candidate) {
					return candidate
				}
			}
		}
	}
	return ""
}

const initConfigTemplate = `# Workbuddy environment configuration
# Each workbuddy instance runs with its own config.

environment: dev          # dev | staging | prod
repo: {{ .Repo }}
poll_interval: 30s
port: 8080                # HTTP audit server port
`
