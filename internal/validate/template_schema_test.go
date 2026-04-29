package validate

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files instead of asserting")

func TestBuildTaskContextSchema_Golden(t *testing.T) {
	schema := BuildTaskContextSchema()
	got := renderSchema(schema)

	goldenPath := filepath.Join("testdata", "template_fields.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("schema diverged from golden — run with -update if intended.\n--- want ---\n%s\n--- got ---\n%s", string(want), got)
	}
}

func TestBuildTaskContextSchema_KnownPaths(t *testing.T) {
	schema := BuildTaskContextSchema()
	for _, want := range []struct {
		path string
		kind FieldKind
	}{
		{"Repo", FieldKindScalar},
		{"Issue", FieldKindStruct},
		{"Issue.Number", FieldKindScalar},
		{"Issue.Title", FieldKindScalar},
		{"Issue.Comments", FieldKindSlice},
		{"Issue.Comments[].Author", FieldKindScalar},
		{"Issue.Comments[].Body", FieldKindScalar},
		{"RelatedPRs", FieldKindSlice},
		{"RelatedPRs[].Number", FieldKindScalar},
		{"PR", FieldKindStruct},
		{"PR.URL", FieldKindScalar},
		{"Session", FieldKindStruct},
		{"Session.ID", FieldKindScalar},
	} {
		got, ok := schema[want.path]
		if !ok {
			t.Errorf("schema missing path %q", want.path)
			continue
		}
		if got != want.kind {
			t.Errorf("schema[%q] = %q, want %q", want.path, got, want.kind)
		}
	}

	// Unexported fields must not leak into the schema.
	for _, banned := range []string{"sessionHandle", "Issue.sessionHandle"} {
		if _, ok := schema[banned]; ok {
			t.Errorf("schema unexpectedly exposes unexported field %q", banned)
		}
	}
}

func renderSchema(s FieldSchema) string {
	var b strings.Builder
	for _, p := range s.SortedPaths() {
		b.WriteString(p)
		b.WriteByte('\t')
		b.WriteString(string(s[p]))
		b.WriteByte('\n')
	}
	return b.String()
}
