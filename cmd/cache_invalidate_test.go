package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestRunCacheInvalidateStore(t *testing.T) {
	t.Run("deletes cache and dependency state", func(t *testing.T) {
		st := newStatusTestStore(t)
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 47, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: "owner/repo", IssueNum: 47, Verdict: store.DependencyVerdictReady}); err != nil {
			t.Fatalf("UpsertIssueDependencyState: %v", err)
		}

		results, err := runCacheInvalidateStore(st, "owner/repo", []int{47}, "test")
		if err != nil {
			t.Fatalf("runCacheInvalidateStore: %v", err)
		}
		if len(results) != 1 || results[0].Result != "deleted" || !results[0].DependencyStateCleared {
			t.Fatalf("unexpected results: %+v", results)
		}
		cache, err := st.QueryIssueCache("owner/repo", 47)
		if err != nil {
			t.Fatalf("QueryIssueCache: %v", err)
		}
		if cache != nil {
			t.Fatalf("cache still present: %+v", cache)
		}
		depState, err := st.QueryIssueDependencyState("owner/repo", 47)
		if err != nil {
			t.Fatalf("QueryIssueDependencyState: %v", err)
		}
		if depState != nil {
			t.Fatalf("dependency state still present: %+v", depState)
		}
	})

	t.Run("graceful when issue not cached", func(t *testing.T) {
		st := newStatusTestStore(t)
		results, err := runCacheInvalidateStore(st, "owner/repo", []int{48}, "test")
		if err != nil {
			t.Fatalf("runCacheInvalidateStore: %v", err)
		}
		if len(results) != 1 || results[0].Result != "skipped" {
			t.Fatalf("unexpected results: %+v", results)
		}
	})

	t.Run("handles missing dependency state", func(t *testing.T) {
		st := newStatusTestStore(t)
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 49, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		results, err := runCacheInvalidateStore(st, "owner/repo", []int{49}, "test")
		if err != nil {
			t.Fatalf("runCacheInvalidateStore: %v", err)
		}
		if len(results) != 1 || results[0].DependencyStateCleared {
			t.Fatalf("unexpected results: %+v", results)
		}
	})

	t.Run("records cache invalidated event", func(t *testing.T) {
		st := newStatusTestStore(t)
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 50, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		if _, err := runCacheInvalidateStore(st, "owner/repo", []int{50}, "test"); err != nil {
			t.Fatalf("runCacheInvalidateStore: %v", err)
		}
		events, err := st.QueryEvents("owner/repo")
		if err != nil {
			t.Fatalf("QueryEvents: %v", err)
		}
		if len(events) != 1 || events[0].Type != "cache_invalidated" || !strings.Contains(events[0].Payload, `"source":"test"`) {
			t.Fatalf("unexpected events: %+v", events)
		}
	})
}

func TestRunCacheInvalidateWithOpts_JSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 47, Labels: `["status:developing"]`, State: "open"}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	_ = st.Close()

	var out bytes.Buffer
	err = runCacheInvalidateWithOpts(context.Background(), &cacheInvalidateOpts{
		repo:    "owner/repo",
		issues:  []int{47},
		dbPath:  dbPath,
		source:  "test",
		jsonOut: true,
	}, &out)
	if err != nil {
		t.Fatalf("runCacheInvalidateWithOpts: %v", err)
	}
	var rows []cacheInvalidateResult
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 || rows[0].Result != "deleted" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}
