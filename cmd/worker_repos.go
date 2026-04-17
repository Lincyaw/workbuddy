package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/workerclient"
	"github.com/spf13/cobra"
)

const (
	defaultWorkerMgmtAddr = "127.0.0.1:0"
	workerAddrFileName    = "worker.addr"
)

type workerRepoBinding struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
}

type workerRepoBindingStore struct {
	mu       sync.RWMutex
	bindings map[string]string
}

func newWorkerRepoBindingStore(initial []workerRepoBinding) *workerRepoBindingStore {
	bindings := make(map[string]string, len(initial))
	for _, binding := range initial {
		bindings[binding.Repo] = binding.Path
	}
	return &workerRepoBindingStore{bindings: bindings}
}

func (s *workerRepoBindingStore) get(repo string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, ok := s.bindings[repo]
	return path, ok
}

func (s *workerRepoBindingStore) list() []workerRepoBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]workerRepoBinding, 0, len(s.bindings))
	for repo, path := range s.bindings {
		out = append(out, workerRepoBinding{Repo: repo, Path: path})
	}
	slices.SortFunc(out, func(a, b workerRepoBinding) int {
		return strings.Compare(a.Repo, b.Repo)
	})
	return out
}

func (s *workerRepoBindingStore) repoNames() []string {
	bindings := s.list()
	out := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, binding.Repo)
	}
	return out
}

func (s *workerRepoBindingStore) set(repo, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[repo] = path
}

func (s *workerRepoBindingStore) delete(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, repo)
}

type workerWorkspaceSet struct {
	mu       sync.Mutex
	managers map[string]workspaceManager
}

type workspaceManager interface {
	Create(issueNum int, taskID string) (string, error)
	Remove(wtPath string) error
	Prune() error
}

func newWorkerWorkspaceSet() *workerWorkspaceSet {
	return &workerWorkspaceSet{managers: make(map[string]workspaceManager)}
}

func (s *workerWorkspaceSet) forRepoPath(repoPath string, create func(string) workspaceManager) workspaceManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mgr, ok := s.managers[repoPath]; ok {
		return mgr
	}
	mgr := create(repoPath)
	if mgr != nil {
		_ = mgr.Prune()
		s.managers[repoPath] = mgr
	}
	return mgr
}

type workerMgmtServer struct {
	server   *http.Server
	listener net.Listener
	addrFile string
}

func startWorkerMgmtServer(mgmtAddr, addrFile string, bindings *workerRepoBindingStore, onChange func(context.Context, []string) error) (*workerMgmtServer, error) {
	if strings.TrimSpace(mgmtAddr) == "" {
		mgmtAddr = defaultWorkerMgmtAddr
	}
	if err := validateWorkerMgmtAddr(mgmtAddr); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(addrFile), 0755); err != nil {
		return nil, fmt.Errorf("worker mgmt: mkdir addr dir: %w", err)
	}
	ln, err := net.Listen("tcp", mgmtAddr)
	if err != nil {
		return nil, fmt.Errorf("worker mgmt: listen %s: %w", mgmtAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mgmt/repos", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeWorkerMgmtJSON(w, http.StatusOK, bindings.list())
		case http.MethodPost:
			var req workerRepoBinding
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeWorkerMgmtJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
				return
			}
			if err := validateWorkerRepoBinding(&req); err != nil {
				writeWorkerMgmtJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			prevPath, existed := bindings.get(req.Repo)
			bindings.set(req.Repo, req.Path)
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := onChange(ctx, bindings.repoNames()); err != nil {
				if existed {
					bindings.set(req.Repo, prevPath)
				} else {
					bindings.delete(req.Repo)
				}
				writeWorkerMgmtJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			writeWorkerMgmtJSON(w, http.StatusOK, req)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/mgmt/repos/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		repo, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/mgmt/repos/"))
		if err != nil || strings.TrimSpace(repo) == "" {
			http.NotFound(w, r)
			return
		}
		prevPath, existed := bindings.get(repo)
		if !existed {
			writeWorkerMgmtJSON(w, http.StatusNotFound, map[string]string{"error": "repo binding not found"})
			return
		}
		bindings.delete(repo)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := onChange(ctx, bindings.repoNames()); err != nil {
			bindings.set(repo, prevPath)
			writeWorkerMgmtJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()

	addr := resolvedWorkerMgmtURL(ln.Addr())
	if err := os.WriteFile(addrFile, []byte(addr+"\n"), 0644); err != nil {
		_ = srv.Shutdown(context.Background())
		_ = ln.Close()
		return nil, fmt.Errorf("worker mgmt: write addr file: %w", err)
	}

	return &workerMgmtServer{
		server:   srv,
		listener: ln,
		addrFile: addrFile,
	}, nil
}

func (s *workerMgmtServer) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	_ = os.Remove(s.addrFile)
	return s.server.Shutdown(ctx)
}

func resolvedWorkerMgmtURL(addr net.Addr) string {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return "http://" + addr.String()
	}
	host := tcpAddr.IP.String()
	if host == "" || host == "::" || host == "<nil>" {
		host = "127.0.0.1"
	}
	if host == "::1" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, tcpAddr.Port)
}

func validateWorkerMgmtAddr(raw string) error {
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		return fmt.Errorf("worker mgmt: invalid addr %q: %w", raw, err)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("worker mgmt: addr must bind to a loopback host")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("worker mgmt: addr must bind to a loopback host")
	}
	return nil
}

func validateWorkerRepoBinding(binding *workerRepoBinding) error {
	if binding == nil {
		return fmt.Errorf("repo binding is required")
	}
	binding.Repo = strings.TrimSpace(binding.Repo)
	binding.Path = strings.TrimSpace(binding.Path)
	if !isOwnerRepo(binding.Repo) {
		return fmt.Errorf("repo must be in OWNER/NAME form")
	}
	if binding.Path == "" {
		return fmt.Errorf("path is required")
	}
	absPath, err := filepath.Abs(binding.Path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path must be a directory")
	}
	binding.Path = absPath
	return nil
}

func isOwnerRepo(repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, " ") {
		return false
	}
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func parseWorkerRepoBinding(raw, defaultPath string, requirePath bool) (workerRepoBinding, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return workerRepoBinding{}, fmt.Errorf("repo binding is required")
	}
	repo, path, hasPath := strings.Cut(raw, "=")
	binding := workerRepoBinding{
		Repo: strings.TrimSpace(repo),
		Path: strings.TrimSpace(path),
	}
	if !hasPath || binding.Path == "" {
		if requirePath {
			return workerRepoBinding{}, fmt.Errorf("repo binding must be in OWNER/NAME=/path form")
		}
		binding.Path = defaultPath
	}
	if err := validateWorkerRepoBinding(&binding); err != nil {
		return workerRepoBinding{}, err
	}
	return binding, nil
}

func parseWorkerRepoBindings(csv, defaultPath string) ([]workerRepoBinding, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]workerRepoBinding, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		binding, err := parseWorkerRepoBinding(part, defaultPath, true)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[binding.Repo]; ok {
			return nil, fmt.Errorf("duplicate repo binding for %s", binding.Repo)
		}
		seen[binding.Repo] = struct{}{}
		out = append(out, binding)
	}
	return out, nil
}

func resolveWorkerRepoBindings(opts *workerOpts, configRepo, defaultPath string) ([]workerRepoBinding, error) {
	if opts == nil {
		return nil, fmt.Errorf("worker: options are required")
	}
	if strings.TrimSpace(opts.reposCSV) != "" && strings.TrimSpace(opts.repo) != "" {
		return nil, fmt.Errorf("worker: --repo and --repos cannot be used together")
	}
	if strings.TrimSpace(opts.reposCSV) != "" {
		return parseWorkerRepoBindings(opts.reposCSV, defaultPath)
	}
	if strings.TrimSpace(opts.repo) != "" {
		binding, err := parseWorkerRepoBinding(opts.repo, defaultPath, false)
		if err != nil {
			return nil, fmt.Errorf("worker: --repo: %w", err)
		}
		return []workerRepoBinding{binding}, nil
	}
	if strings.TrimSpace(configRepo) != "" {
		binding, err := parseWorkerRepoBinding(configRepo, defaultPath, false)
		if err != nil {
			return nil, fmt.Errorf("worker: config repo: %w", err)
		}
		return []workerRepoBinding{binding}, nil
	}
	return nil, fmt.Errorf("worker: repo is required")
}

func registerWorkerRepos(ctx context.Context, client *workerclient.Client, workerID string, roles []string, runtime string, bindings []workerRepoBinding) error {
	if len(bindings) == 0 {
		return fmt.Errorf("worker: at least one repo binding is required")
	}
	repos := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		repos = append(repos, binding.Repo)
	}
	return client.Register(ctx, workerclient.RegisterRequest{
		WorkerID: workerID,
		Repo:     repos[0],
		Roles:    roles,
		Runtime:  runtime,
		Repos:    repos,
		Hostname: hostnameOrUnknown(),
	})
}

func workerAddrFile(controlDir string) string {
	return filepath.Join(controlDir, ".workbuddy", workerAddrFileName)
}

type workerMgmtClient struct {
	baseURL    string
	httpClient *http.Client
}

func newWorkerMgmtClient(baseURL string) *workerMgmtClient {
	return &workerMgmtClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func workerMgmtClientFromControlDir(controlDir string) (*workerMgmtClient, error) {
	addrBytes, err := os.ReadFile(workerAddrFile(controlDir))
	if err != nil {
		return nil, fmt.Errorf("worker repos: read worker addr: %w", err)
	}
	baseURL := strings.TrimSpace(string(addrBytes))
	if baseURL == "" {
		return nil, fmt.Errorf("worker repos: worker addr file is empty")
	}
	return newWorkerMgmtClient(baseURL), nil
}

func (c *workerMgmtClient) List(ctx context.Context) ([]workerRepoBinding, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/mgmt/repos", nil)
	if err != nil {
		return nil, err
	}
	var bindings []workerRepoBinding
	if err := c.do(req, http.StatusOK, &bindings); err != nil {
		return nil, err
	}
	return bindings, nil
}

func (c *workerMgmtClient) Add(ctx context.Context, binding workerRepoBinding) error {
	body, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mgmt/repos", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, http.StatusOK, nil)
}

func (c *workerMgmtClient) Remove(ctx context.Context, repo string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/mgmt/repos/"+url.PathEscape(repo), nil)
	if err != nil {
		return err
	}
	return c.do(req, http.StatusNoContent, nil)
}

func (c *workerMgmtClient) do(req *http.Request, wantStatus int, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		var payload map[string]string
		if json.Unmarshal(body, &payload) == nil && payload["error"] != "" {
			return fmt.Errorf("worker repos: %s", payload["error"])
		}
		return fmt.Errorf("worker repos: unexpected status %d", resp.StatusCode)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return err
		}
	}
	return nil
}

var workerReposCmd = &cobra.Command{
	Use:   "repos",
	Short: "Manage repo bindings for a running worker",
}

var workerReposAddCmd = &cobra.Command{
	Use:   "add <owner/repo>=</local/path>",
	Short: "Add a repo binding to a running worker",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		controlDir, err := os.Getwd()
		if err != nil {
			return err
		}
		client, err := workerMgmtClientFromControlDir(controlDir)
		if err != nil {
			return err
		}
		binding, err := parseWorkerRepoBinding(args[0], controlDir, true)
		if err != nil {
			return err
		}
		return client.Add(cmd.Context(), binding)
	},
}

var workerReposRemoveCmd = &cobra.Command{
	Use:   "remove <owner/repo>",
	Short: "Remove a repo binding from a running worker",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		controlDir, err := os.Getwd()
		if err != nil {
			return err
		}
		client, err := workerMgmtClientFromControlDir(controlDir)
		if err != nil {
			return err
		}
		repo := strings.TrimSpace(args[0])
		if !isOwnerRepo(repo) {
			return fmt.Errorf("repo must be in OWNER/NAME form")
		}
		return client.Remove(cmd.Context(), repo)
	},
}

var workerReposListCmd = &cobra.Command{
	Use:   "list",
	Short: "List repo bindings for a running worker",
	RunE: func(cmd *cobra.Command, _ []string) error {
		controlDir, err := os.Getwd()
		if err != nil {
			return err
		}
		client, err := workerMgmtClientFromControlDir(controlDir)
		if err != nil {
			return err
		}
		bindings, err := client.List(cmd.Context())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "REPO\tPATH")
		for _, binding := range bindings {
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", binding.Repo, binding.Path)
		}
		return tw.Flush()
	},
}

func writeWorkerMgmtJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}
