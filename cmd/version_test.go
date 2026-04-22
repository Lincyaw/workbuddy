package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRunVersionWithOpts_JSON(t *testing.T) {
	prevVersion, prevCommit, prevDate := Version, Commit, Date
	Version = "v9.9.9"
	Commit = "deadbeef"
	Date = "2026-04-22T00:00:00Z"
	t.Cleanup(func() {
		Version = prevVersion
		Commit = prevCommit
		Date = prevDate
	})

	var out bytes.Buffer
	if err := runVersionWithOpts(&versionOpts{format: outputFormatJSON}, &out); err != nil {
		t.Fatalf("runVersionWithOpts: %v", err)
	}

	var got versionResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Version != "v9.9.9" || got.Commit != "deadbeef" || got.Date != "2026-04-22T00:00:00Z" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}
