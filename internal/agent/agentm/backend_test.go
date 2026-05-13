package agentm_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm/agentmtest"
)

func newSpec(t *testing.T) agent.Spec {
	t.Helper()
	work := t.TempDir()
	return agent.Spec{
		Backend: "agentm",
		Workdir: work,
		Prompt:  "do the thing",
		Env: map[string]string{
			"WORKBUDDY_ISSUE_NUMBER": "319",
			"WORKBUDDY_REPO":         "Lincyaw/workbuddy",
			"WORKBUDDY_SESSION_ID":   "test-session",
			"TRACEPARENT":            "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
	}
}

func TestBackend_HappyPath_Success(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// Drain events in the background; the channel must close.
	doneEvt := make(chan struct{})
	go func() {
		for range sess.Events() {
		}
		close(doneEvt)
	}()
	res, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	<-doneEvt

	extractor, ok := sess.(interface {
		Output() (*agentm.Output, error)
		SessionLogPath() string
	})
	if !ok {
		t.Fatalf("session does not expose Output()")
	}
	out, perr := extractor.Output()
	if perr != nil {
		t.Fatalf("Output(): %v", perr)
	}
	if !out.Success {
		t.Fatalf("expected success=true")
	}
	if out.NextLabel != "status:review" {
		t.Fatalf("expected next_label=status:review, got %q", out.NextLabel)
	}
	logPath := extractor.SessionLogPath()
	if logPath == "" {
		t.Fatalf("expected session log path")
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("session log missing: %v", err)
	}
}

func TestBackend_MalformedJSON_IsInfraFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeMalformedJSON})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error for malformed RESULT")
	}
	extractor := sess.(interface {
		Output() (*agentm.Output, error)
	})
	out, perr := extractor.Output()
	if perr == nil {
		t.Fatalf("expected Output() parse error, got out=%+v", out)
	}
}

func TestBackend_MissingRequired_SchemaError(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeMissingRequired})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error for missing required field")
	}
}

func TestBackend_Failure_SurfacesReason(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:          agentmtest.ModeFailure,
		FailureReason: "tests fail",
		NextLabel:     "status:failed",
	})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error for task failure")
	}
	extractor := sess.(interface {
		Output() (*agentm.Output, error)
	})
	out, perr := extractor.Output()
	if perr != nil {
		t.Fatalf("Output(): %v", perr)
	}
	if out.Success {
		t.Fatalf("expected success=false")
	}
	if out.FailureReason != "tests fail" {
		t.Fatalf("got failure_reason=%q", out.FailureReason)
	}
	if out.NextLabel != "status:failed" {
		t.Fatalf("got next_label=%q", out.NextLabel)
	}
}

func TestBackend_NoResult_IsInfraFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeNoResult})
	be := &agentm.Backend{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := be.NewSession(ctx, newSpec(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	_, err = sess.Wait(ctx)
	if err == nil {
		t.Fatalf("expected wait error when no RESULT emitted")
	}
}

func TestParseAndValidate_RejectsBadLabel(t *testing.T) {
	_, err := agentm.ParseAndValidate([]byte(`{"success":true,"next_label":"NotAStatus","session_log_path":"/tmp/x"}`))
	if err == nil {
		t.Fatalf("expected schema error for invalid next_label pattern")
	}
}

func TestSchemaEmbedMatchesRepo(t *testing.T) {
	// Guardrail: keep the embedded schema in lockstep with the canonical
	// schemas/agentm-output.schema.json so contributors who touch one are
	// nudged to touch the other.
	repoSchema, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "agentm-output.schema.json"))
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	embedded, err := os.ReadFile("agentm-output.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	if string(repoSchema) != string(embedded) {
		t.Fatalf("internal/agent/agentm/agentm-output.schema.json drifted from schemas/agentm-output.schema.json; please re-copy")
	}
}
