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
		current            State
		allowed            []State
		wantClassification Classification
		wantFrom           string
		wantTo             string
	}{
		{
			name:               "allowed transition",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:reviewing"},
			exitCode:           0,
			current:            State{Name: "developing", Label: "status:developing"},
			allowed:            allowed,
			wantClassification: ClassificationOK,
			wantFrom:           "developing",
			wantTo:             "reviewing",
		},
		{
			name:               "no transition after success",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:developing"},
			exitCode:           0,
			current:            State{Name: "developing", Label: "status:developing"},
			allowed:            allowed,
			wantClassification: ClassificationNoTransitionAfterSuccess,
			wantFrom:           "developing",
			wantTo:             "developing",
		},
		{
			name:               "no transition after failure",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:developing"},
			exitCode:           1,
			current:            State{Name: "developing", Label: "status:developing"},
			allowed:            allowed,
			wantClassification: ClassificationNoTransitionAfterFailure,
			wantFrom:           "developing",
			wantTo:             "developing",
		},
		{
			name:               "unexpected transition",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:done"},
			exitCode:           0,
			current:            State{Name: "developing", Label: "status:developing"},
			allowed:            allowed,
			wantClassification: ClassificationUnexpectedTransition,
			wantFrom:           "developing",
			wantTo:             "done",
		},
		{
			name:               "failed label wins",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:failed"},
			exitCode:           1,
			current:            State{Name: "developing", Label: "status:developing"},
			allowed:            allowed,
			wantClassification: ClassificationFailed,
			wantFrom:           "developing",
			wantTo:             "failed",
		},
		{
			name:               "pre snapshot overrides queued state",
			pre:                []string{"workbuddy", "status:reviewing"},
			post:               []string{"workbuddy", "status:reviewing"},
			exitCode:           0,
			current:            State{Name: "developing", Label: "status:developing"},
			allowed:            []State{{Name: "done", Label: "status:done"}},
			wantClassification: ClassificationNoTransitionAfterSuccess,
			wantFrom:           "reviewing",
			wantTo:             "reviewing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(Input{
				Pre:                tt.pre,
				Post:               tt.post,
				ExitCode:           tt.exitCode,
				Current:            tt.current,
				AllowedTransitions: tt.allowed,
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
		})
	}
}

func TestResolveCurrent(t *testing.T) {
	knownStates := []State{
		{Name: "developing", Label: "status:developing"},
		{Name: "reviewing", Label: "status:reviewing"},
	}

	tests := []struct {
		name     string
		pre      []string
		fallback State
		want     string
	}{
		{
			name:     "prefers pre snapshot state over queued fallback",
			pre:      []string{"status:reviewing"},
			fallback: State{Name: "developing", Label: "status:developing"},
			want:     "reviewing",
		},
		{
			name:     "falls back when pre snapshot has no known state",
			pre:      []string{"workbuddy"},
			fallback: State{Name: "developing", Label: "status:developing"},
			want:     "developing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveCurrent(tt.pre, tt.fallback, knownStates)
			if got.Name != tt.want {
				t.Fatalf("ResolveCurrent(...).Name = %q, want %q", got.Name, tt.want)
			}
		})
	}
}
