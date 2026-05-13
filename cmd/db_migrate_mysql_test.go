//go:build mysql_integration

// Integration variant of TestDBMigrateHappyPath that targets a real MySQL
// destination. Gated by the mysql_integration build tag and skipped unless
// WORKBUDDY_MYSQL_TEST_DSN is set, mirroring the convention in
// internal/store/store_mysql_integration_test.go.
//
// Example:
//
//	WORKBUDDY_MYSQL_TEST_DSN='mysql://root:root@tcp(127.0.0.1:3306)/workbuddy_migrate_test' \
//	  go test -tags mysql_integration ./cmd/... -run TestDBMigrateToMySQL -count=1
//
// See internal/store/mysql/README.md for the docker-compose snippet that
// stands up the matching MySQL container.
package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestDBMigrateToMySQL(t *testing.T) {
	dsn := os.Getenv("WORKBUDDY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("WORKBUDDY_MYSQL_TEST_DSN not set; skipping real-MySQL migrate test")
	}

	dir := t.TempDir()
	srcDSN := "sqlite://" + filepath.Join(dir, "src.db")

	src := mustOpen(t, srcDSN)
	seedRepresentative(t, src)
	_ = src.Close()

	// Wipe destination so re-runs of this test stay deterministic.
	dst := mustOpen(t, dsn)
	if err := wipeDestination(dst); err != nil {
		t.Fatalf("pre-wipe destination: %v", err)
	}
	_ = dst.Close()

	var progress bytes.Buffer
	if err := runDBMigrate(context.Background(), dbMigrateOpts{
		from: srcDSN,
		to:   dsn,
	}, &progress); err != nil {
		t.Fatalf("runDBMigrate: %v\n%s", err, progress.String())
	}

	dst = mustOpen(t, dsn)
	defer func() { _ = dst.Close() }()

	got, err := dst.QueryIssueCache("owner/repo", 318)
	if err != nil {
		t.Fatalf("QueryIssueCache: %v", err)
	}
	if got == nil {
		t.Fatalf("issue_cache row missing on MySQL after migrate")
	}
	_ = store.IssueCache{} // silence unused-import warning if struct ref ever drops
}
