package validatedocs

import (
	"path/filepath"
	"testing"
)

func TestValidateRepo_LayerFixtures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		fixture   string
		wantCodes []string
		wantNone  []string
	}{
		{
			name:     "L1 pass",
			fixture:  "l1-pass",
			wantNone: []string{CodeMissingCodePath, CodeMissingTestPath, CodeMissingRelatedDocPath, CodeMissingSkillReference},
		},
		{
			name:    "L1 fail",
			fixture: "l1-fail",
			wantCodes: []string{
				CodeMissingCodePath,
				CodeMissingTestPath,
				CodeMissingRelatedDocPath,
				CodeMissingSkillReference,
			},
		},
		{
			name:     "L2 pass",
			fixture:  "l2-pass",
			wantNone: []string{CodePluginSyncDrift},
		},
		{
			name:      "L2 fail",
			fixture:   "l2-fail",
			wantCodes: []string{CodePluginSyncDrift},
		},
		{
			name:     "L3 pass",
			fixture:  "l3-pass",
			wantNone: []string{CodeAgentFrontmatterMismatch, CodeDuplicateSkillDivergence},
		},
		{
			name:      "L3 fail",
			fixture:   "l3-fail",
			wantCodes: []string{CodeAgentFrontmatterMismatch, CodeDuplicateSkillDivergence},
		},
		{
			name:     "L4 pass",
			fixture:  "l4-pass",
			wantNone: []string{CodeSkillNameMismatch, CodeSkillDescriptionTooShort, CodeSkillFlagEnumeration},
		},
		{
			name:      "L4 fail",
			fixture:   "l4-fail",
			wantCodes: []string{CodeSkillNameMismatch, CodeSkillDescriptionTooShort, CodeSkillFlagEnumeration},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join("testdata", tc.fixture)
			diags, err := ValidateRepo(root)
			if err != nil {
				t.Fatalf("ValidateRepo: %v", err)
			}
			codes := map[string]bool{}
			for _, diag := range diags {
				codes[diag.Code] = true
			}
			for _, want := range tc.wantCodes {
				if !codes[want] {
					t.Fatalf("expected diagnostic %s, got %#v", want, diags)
				}
			}
			for _, forbidden := range tc.wantNone {
				if codes[forbidden] {
					t.Fatalf("unexpected diagnostic %s in %#v", forbidden, diags)
				}
			}
		})
	}
}
