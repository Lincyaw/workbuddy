package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/store"
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
	Create(issueNum int, taskID string, rolloutIndex int) (string, error)
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
	baseURL  string
}

func startWorkerMgmtServer(mgmtAddr, addrFile, authToken string, bindings *workerRepoBindingStore, st *store.Store, sessionsDir string, onChange func(context.Context, []string) error, onConfigReload func(context.Context) (any, error)) (*workerMgmtServer, error) {
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
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	protected := func(next http.Handler) http.Handler {
		return wrapBearerAuth(authToken, next)
	}
	mux.Handle("/mgmt/repos", protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})))
	mux.Handle("/mgmt/config/reload", protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if onConfigReload == nil {
			writeWorkerMgmtJSON(w, http.StatusNotImplemented, map[string]string{"error": "config reload is unavailable"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		summary, err := onConfigReload(ctx)
		if err != nil {
			writeWorkerMgmtJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeWorkerMgmtJSON(w, http.StatusOK, summary)
	})))
	mux.Handle("/mgmt/repos/", protected(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})))

	if st != nil {
		sessionAPI := auditapi.NewHandler(st)
		sessionAPI.SetSessionsDir(sessionsDir)
		sessionAPIMux := http.NewServeMux()
		sessionAPI.RegisterSessionsOnly(sessionAPIMux)
		mux.Handle("/sessions/", protected(sessionAPIMux))
	}

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
		baseURL:  addr,
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

func wrapBearerAuth(token string, next http.Handler) http.Handler {
	token = strings.TrimSpace(token)
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) || strings.TrimSpace(strings.TrimPrefix(authz, prefix)) != token {
			writeWorkerMgmtJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validateWorkerMgmtAddr(raw string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("worker mgmt: invalid addr %q: %w", raw, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("worker mgmt: addr must include a host, got %q", raw)
	}
	return nil
}

// validateWorkerMgmtBind enforces the policy for the worker management
// listener. The mgmt server publishes the per-worker session viewer that the
// coordinator proxies to (so issue-comment session links are resolvable). A
// non-loopback bind is only safe when --mgmt-public-url advertises a host that
// is reachable from the operator's browser; otherwise comment links would be
// unclickable. The function intentionally returns the actionable error
// suggested in #221.
func validateWorkerMgmtBind(mgmtAddr, mgmtPublicURL string) error {
	if strings.TrimSpace(mgmtAddr) == "" {
		// Empty mgmt-addr is filled in by startWorkerMgmtServer with the
		// loopback default; no policy check is needed here.
		return nil
	}
	if err := validateWorkerMgmtAddr(mgmtAddr); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(mgmtAddr))
	if err != nil {
		return fmt.Errorf("worker mgmt: invalid addr %q: %w", mgmtAddr, err)
	}
	if isLoopbackMgmtHost(host) {
		return nil
	}
	publicURL := strings.TrimSpace(mgmtPublicURL)
	if publicURL == "" {
		return fmt.Errorf(
			"worker: --mgmt-addr %s is non-loopback but --mgmt-public-url is missing.\n"+
				"Session links proxied through the coordinator would be unreachable from a browser.\n"+
				"Pass --mgmt-public-url=http://<your-worker-host>:%s (or set WORKBUDDY_REPORT_BASE_URL in the deploy env) to fix.",
			mgmtAddr, port,
		)
	}
	if isLoopbackReportBaseURL(publicURL) {
		return fmt.Errorf(
			"worker: --mgmt-addr %s is non-loopback but --mgmt-public-url is loopback (%s).\n"+
				"Session links proxied through the coordinator would be unreachable from a browser.\n"+
				"Pass --mgmt-public-url=http://<your-worker-host>:%s to fix.",
			mgmtAddr, publicURL, port,
		)
	}
	return nil
}

func isLoopbackMgmtHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateWorkerMgmtPublicURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("worker: invalid --mgmt-public-url %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("worker: --mgmt-public-url must use http or https")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("worker: --mgmt-public-url must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("worker: --mgmt-public-url must not include query or fragment")
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
	if strings.TrimSpace(opts.reposCSV) != "" {
		return parseWorkerRepoBindings(opts.reposCSV, defaultPath)
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

func registerWorkerRepos(ctx context.Context, client *workerclient.Client, workerID string, roles []string, runtime, mgmtBaseURL string, bindings []workerRepoBinding) error {
	if len(bindings) == 0 {
		return fmt.Errorf("worker: at least one repo binding is required")
	}
	repos := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		repos = append(repos, binding.Repo)
	}
	return client.Register(ctx, workerclient.RegisterRequest{
		WorkerID:    workerID,
		Repo:        repos[0],
		Roles:       roles,
		Runtime:     runtime,
		Repos:       repos,
		Hostname:    hostnameOrUnknown(),
		MgmtBaseURL: strings.TrimRight(strings.TrimSpace(mgmtBaseURL), "/"),
	})
}

func workerAddrFile(controlDir string) string {
	return filepath.Join(controlDir, ".workbuddy", workerAddrFileName)
}

type workerMgmtClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

func newWorkerMgmtClient(baseURL string) *workerMgmtClient {
	return &workerMgmtClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		authToken:  defaultWorkerMgmtAuthToken(""),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func workerMgmtClientFromControlDir(controlDir string) (*workerMgmtClient, error) {
	addrPath := workerAddrFile(controlDir)
	addrBytes, err := os.ReadFile(addrPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, actionableError("worker repos", fmt.Sprintf("worker control file %q was not found", addrPath), "Start `workbuddy worker` in this repo before managing repo bindings")
		}
		return nil, fmt.Errorf("worker repos: read worker addr %q: %w", addrPath, err)
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

func (c *workerMgmtClient) Reload(ctx context.Context) (*workerConfigReloadSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mgmt/config/reload", nil)
	if err != nil {
		return nil, err
	}
	var summary workerConfigReloadSummary
	if err := c.do(req, http.StatusOK, &summary); err != nil {
		return nil, err
	}
	return &summary, nil
}

func (c *workerMgmtClient) do(req *http.Request, wantStatus int, out any) error {
	if strings.TrimSpace(c.authToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.authToken))
	}
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
		if err := requireWritable(cmd, "worker repos add"); err != nil {
			return err
		}
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
		if err := requireWritable(cmd, "worker repos remove"); err != nil {
			return err
		}
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
		format, err := resolveOutputFormat(cmd, "worker repos list")
		if err != nil {
			return err
		}
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
		if isJSONOutput(format) {
			return writeJSON(cmdStdout(cmd), bindings)
		}
		tw := tabwriter.NewWriter(cmdStdout(cmd), 0, 4, 2, ' ', 0)
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
