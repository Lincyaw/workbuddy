package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestRunDiagnoseWithOpts(t *testing.T) {
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	t.Run("healthy pipeline", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "healthy.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		_ = st.Close()

		var out bytes.Buffer
		err = runDiagnoseWithOpts(context.Background(), &diagnoseOpts{
			dbPath: dbPath,
			now:    func() time.Time { return now },
		}, &out)
		if err != nil {
			t.Fatalf("runDiagnoseWithOpts: %v", err)
		}
		if strings.TrimSpace(out.String()) != "Pipeline healthy: no issues detected" {
			t.Fatalf("unexpected output: %q", out.String())
		}
	})

	t.Run("fix applies cache invalidate and auto_fix event", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "fix.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 47, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: "owner/repo", IssueNum: 47, Verdict: store.DependencyVerdictReady}); err != nil {
			t.Fatalf("UpsertIssueDependencyState: %v", err)
		}
		if _, err := st.InsertEvent(store.Event{Type: "state_entry", Repo: "owner/repo", IssueNum: 47, Payload: `{"state":"status:developing"}`}); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
		_ = st.Close()

		var out bytes.Buffer
		err = runDiagnoseWithOpts(context.Background(), &diagnoseOpts{
			repo:    "owner/repo",
			dbPath:  dbPath,
			fix:     true,
			jsonOut: true,
			now:     func() time.Time { return now },
		}, &out)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %v", err)
		}

		var rows []diagnoseResult
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rows) == 0 || !rows[0].FixApplied {
			t.Fatalf("expected applied fix, got %+v", rows)
		}

		st2, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer func() { _ = st2.Close() }()
		cache, err := st2.QueryIssueCache("owner/repo", 47)
		if err != nil {
			t.Fatalf("QueryIssueCache: %v", err)
		}
		if cache != nil {
			t.Fatalf("cache still present: %+v", cache)
		}
		events, err := st2.QueryEvents("owner/repo")
		if err != nil {
			t.Fatalf("QueryEvents: %v", err)
		}
		gotAutoFix := false
		for _, event := range events {
			if event.Type == "auto_fix" {
				gotAutoFix = true
			}
		}
		if !gotAutoFix {
			t.Fatalf("expected auto_fix event, got %+v", events)
		}
	})
}
