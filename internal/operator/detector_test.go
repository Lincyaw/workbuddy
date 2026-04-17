package operator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type staticProcessInspector struct {
	count int
}

func (s staticProcessInspector) CodexProcessCount(context.Context) (int, error) {
	return s.count, nil
}

func newOperatorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "operator.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newDetectorForTest(t *testing.T, st *store.Store, now time.Time) (*Detector, string) {
	t.Helper()
	inboxDir := filepath.Join(t.TempDir(), "inbox")
	return NewDetector(DetectorOptions{
		Store: st,
		Config: config.OperatorConfig{
			Enabled:       true,
			CheckInterval: time.Minute,
			DedupWindow:   5 * time.Minute,
			InboxDir:      inboxDir,
		},
		DefaultRepo:             "owner/repo",
		DefaultPollInterval:     30 * time.Second,
		WorkerHeartbeatInterval: 15 * time.Second,
		ProcessInspector:        staticProcessInspector{},
		Now:                     func() time.Time { return now },
	}), inboxDir
}

func TestDetectorLeaseExpired(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-1",
		Repo:      "owner/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE task_queue SET lease_expires_at = ? WHERE id = ?`, now.Add(-time.Minute).Format("2006-01-02 15:04:05"), "task-1"); err != nil {
		t.Fatalf("update lease_expires_at: %v", err)
	}

	detector, _ := newDetectorForTest(t, st, now)
	alerts, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("runPass: %v", err)
	}
	requireAlertKind(t, alerts, KindLeaseExpired)
}

func TestDetectorTaskStuck(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-2",
		Repo:      "owner/repo",
		IssueNum:  2,
		AgentName: "dev-agent",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE task_queue SET created_at = ? WHERE id = ?`, now.Add(-11*time.Minute).Format("2006-01-02 15:04:05"), "task-2"); err != nil {
		t.Fatalf("update created_at: %v", err)
	}

	detector, _ := newDetectorForTest(t, st, now)
	alerts, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("runPass: %v", err)
	}
	requireAlertKind(t, alerts, KindTaskStuck)
}

func TestDetectorMissingLabel(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 3,
		Labels:   `["workbuddy","type:feature"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE issue_cache SET updated_at = ? WHERE repo = ? AND issue_num = ?`, now.Add(-6*time.Minute).Format("2006-01-02 15:04:05"), "owner/repo", 3); err != nil {
		t.Fatalf("update issue_cache: %v", err)
	}

	detector, _ := newDetectorForTest(t, st, now)
	alerts, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("runPass: %v", err)
	}
	requireAlertKind(t, alerts, KindMissingLabel)
}

func TestDetectorCacheStale(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 4,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE issue_cache SET updated_at = ? WHERE repo = ? AND issue_num = ?`, now.Add(-2*time.Minute).Format("2006-01-02 15:04:05"), "owner/repo", 4); err != nil {
		t.Fatalf("update issue_cache: %v", err)
	}

	detector, _ := newDetectorForTest(t, st, now)
	alerts, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("runPass: %v", err)
	}
	requireAlertKind(t, alerts, KindCacheStale)
}

func TestDetectorWorkerMissing(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-1",
		Repo:     "owner/repo",
		Roles:    `["dev"]`,
		Hostname: "host-1",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE workers SET last_heartbeat = ? WHERE id = ?`, now.Add(-time.Minute).Format("2006-01-02 15:04:05"), "worker-1"); err != nil {
		t.Fatalf("update worker heartbeat: %v", err)
	}

	detector, _ := newDetectorForTest(t, st, now)
	alerts, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("runPass: %v", err)
	}
	requireAlertKind(t, alerts, KindWorkerMissing)
}

func TestDetectorCoordinatorRestartGap(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 6,
		Labels:   `["workbuddy","status:reviewing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}

	detector, _ := newDetectorForTest(t, st, now)
	alerts, err := detector.runPass(context.Background(), true)
	if err != nil {
		t.Fatalf("runPass: %v", err)
	}
	requireAlertKind(t, alerts, KindCoordinatorRestartGap)
}

func TestDetectorDedupAndInboxPermissions(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 7,
		Labels:   `["workbuddy","type:feature"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE issue_cache SET updated_at = ? WHERE repo = ? AND issue_num = ?`, now.Add(-6*time.Minute).Format("2006-01-02 15:04:05"), "owner/repo", 7); err != nil {
		t.Fatalf("update issue_cache: %v", err)
	}

	detector := NewDetector(DetectorOptions{
		Store: st,
		Config: config.OperatorConfig{
			Enabled:       true,
			CheckInterval: time.Minute,
			DedupWindow:   5 * time.Minute,
			InboxDir:      filepath.Join(t.TempDir(), "inbox"),
		},
		DefaultRepo:             "owner/repo",
		DefaultPollInterval:     10 * time.Minute,
		WorkerHeartbeatInterval: 15 * time.Second,
		ProcessInspector:        staticProcessInspector{},
		Now:                     func() time.Time { return now },
	})
	inboxDir := detector.cfg.InboxDir
	first, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("first runPass: %v", err)
	}
	if len(first) != 1 || first[0].Kind != KindMissingLabel {
		t.Fatalf("first alerts = %+v, want one missing_label", first)
	}
	second, err := detector.runPass(context.Background(), false)
	if err != nil {
		t.Fatalf("second runPass: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second alerts = %+v, want dedup suppression", second)
	}

	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	alertEvents := 0
	for _, event := range events {
		if event.Type == eventlog.TypeAlert {
			alertEvents++
		}
	}
	if alertEvents != 1 {
		t.Fatalf("alert event count = %d, want 1", alertEvents)
	}

	dirInfo, err := os.Stat(inboxDir)
	if err != nil {
		t.Fatalf("Stat(inboxDir): %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("inbox dir perms = %o, want 700", dirInfo.Mode().Perm())
	}

	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		t.Fatalf("ReadDir(inboxDir): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox file count = %d, want 1", len(entries))
	}
	filePath := filepath.Join(inboxDir, entries[0].Name())
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat(file): %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("inbox file perms = %o, want 600", fileInfo.Mode().Perm())
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile(file): %v", err)
	}
	var alert Alert
	if err := json.Unmarshal(data, &alert); err != nil {
		t.Fatalf("Unmarshal(alert): %v", err)
	}
	if alert.Kind != KindMissingLabel {
		t.Fatalf("alert.Kind = %q, want %q", alert.Kind, KindMissingLabel)
	}
	if !strings.HasSuffix(filePath, alert.ID+".json") {
		t.Fatalf("file path %q does not match alert id %q", filePath, alert.ID)
	}
}

func TestDetectorDisabledSkipsEmission(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	st := newOperatorStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 8,
		Labels:   `["workbuddy","type:feature"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE issue_cache SET updated_at = ? WHERE repo = ? AND issue_num = ?`, now.Add(-6*time.Minute).Format("2006-01-02 15:04:05"), "owner/repo", 8); err != nil {
		t.Fatalf("update issue_cache: %v", err)
	}

	detector := NewDetector(DetectorOptions{
		Store: st,
		Config: config.OperatorConfig{
			Enabled:  false,
			InboxDir: filepath.Join(t.TempDir(), "inbox"),
		},
		DefaultRepo:         "owner/repo",
		DefaultPollInterval: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err := detector.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	for _, event := range events {
		if event.Type == eventlog.TypeAlert {
			t.Fatalf("unexpected alert event: %+v", event)
		}
	}
}

func requireAlertKind(t *testing.T, alerts []Alert, kind string) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Kind == kind {
			return
		}
	}
	t.Fatalf("alerts missing kind %q: %+v", kind, alerts)
}
