package labelcheck

import "testing"

func TestClassify(t *testing.T) {
	knownStates := []State{
		{Name: "developing", Label: "status:developing"},
		{Name: "reviewing", Label: "status:reviewing"},
		{Name: "done", Label: "status:done"},
		{Name: "failed", Label: "status:failed"},
	}
	allowed := []State{
		{Name: "reviewing", Label: "status:reviewing"},
	}

	tests := []struct {
		name               string
		pre                []string
		post               []string
		exitCode           int
		wantClassification Classification
		wantFrom           string
		wantTo             string
		wantSummary        string
	}{
		{
			name:               "allowed transition",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:reviewing"},
			exitCode:           0,
			wantClassification: ClassificationOK,
			wantFrom:           "developing",
			wantTo:             "reviewing",
			wantSummary:        "Label transition: developing -> reviewing (OK)",
		},
		{
			name:               "no transition after success",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:developing"},
			exitCode:           0,
			wantClassification: ClassificationNoTransitionAfterSuccess,
			wantFrom:           "developing",
			wantTo:             "developing",
			wantSummary:        "Label transition: none - needs human review",
		},
		{
			name:               "no transition after failure",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:developing"},
			exitCode:           1,
			wantClassification: ClassificationNoTransitionAfterFailure,
			wantFrom:           "developing",
			wantTo:             "developing",
			wantSummary:        "Label transition: none - retry path",
		},
		{
			name:               "unexpected transition",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:done"},
			exitCode:           0,
			wantClassification: ClassificationUnexpectedTransition,
			wantFrom:           "developing",
			wantTo:             "done",
			wantSummary:        "Label transition: developing -> done (unexpected)",
		},
		{
			name:               "failed label wins",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:failed"},
			exitCode:           1,
			wantClassification: ClassificationFailed,
			wantFrom:           "developing",
			wantTo:             "failed",
			wantSummary:        "Label transition: developing -> failed (failed)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(Input{
				Pre:                tt.pre,
				Post:               tt.post,
				ExitCode:           tt.exitCode,
				Current:            State{Name: "developing", Label: "status:developing"},
				AllowedTransitions: allowed,
				KnownStates:        knownStates,
			})

			if got.Classification != tt.wantClassification {
				t.Fatalf("Classification = %q, want %q", got.Classification, tt.wantClassification)
			}
			if got.From.Name != tt.wantFrom {
				t.Fatalf("From = %q, want %q", got.From.Name, tt.wantFrom)
			}
			if got.To.Name != tt.wantTo {
				t.Fatalf("To = %q, want %q", got.To.Name, tt.wantTo)
			}
			if got.Summary() != tt.wantSummary {
				t.Fatalf("Summary = %q, want %q", got.Summary(), tt.wantSummary)
			}
		})
	}
}
