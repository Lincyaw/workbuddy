package config

import (
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/liveness"
)

// TestApplyWorkerDefaultsDeriveFromLiveness proves the config loader is NOT a
// second source of truth for the staleinference thresholds: the defaults it
// injects equal liveness.Default(), so a future liveness edit provably flows
// to the production worker path (ADR 2026-06-06 §6).
func TestApplyWorkerDefaultsDeriveFromLiveness(t *testing.T) {
	dl := liveness.Default()
	var cfg WorkerConfig
	applyWorkerDefaults(&cfg)
	if cfg.StaleInference.IdleThreshold != dl.IdleKill {
		t.Errorf("IdleThreshold default = %s, want shared %s", cfg.StaleInference.IdleThreshold, dl.IdleKill)
	}
	if cfg.StaleInference.CheckInterval != dl.IdleCheckInterval {
		t.Errorf("CheckInterval default = %s, want shared %s", cfg.StaleInference.CheckInterval, dl.IdleCheckInterval)
	}
	if cfg.StaleInference.CompletedGracePeriod != dl.CompletedGracePeriod {
		t.Errorf("CompletedGracePeriod default = %s, want shared %s", cfg.StaleInference.CompletedGracePeriod, dl.CompletedGracePeriod)
	}
}

// TestApplyWorkerDefaultsOverrideFlows verifies explicit
// worker.stale_inference.* values stay authoritative and are not overwritten
// by the liveness-derived defaults.
func TestApplyWorkerDefaultsOverrideFlows(t *testing.T) {
	cfg := WorkerConfig{
		StaleInference: StaleInferenceConfig{
			IdleThreshold:        7 * time.Minute,
			CheckInterval:        5 * time.Second,
			CompletedGracePeriod: 42 * time.Second,
		},
	}
	want := cfg.StaleInference
	applyWorkerDefaults(&cfg)
	if cfg.StaleInference != want {
		t.Fatalf("explicit stale_inference config must win: got %+v, want %+v", cfg.StaleInference, want)
	}
}

// TestApplyOperatorDefaultsDeriveFromLiveness proves the operator scan
// interval default comes from the shared liveness source of truth.
func TestApplyOperatorDefaultsDeriveFromLiveness(t *testing.T) {
	var cfg OperatorConfig
	applyOperatorDefaults(&cfg, false)
	if cfg.CheckInterval != liveness.Default().AlertCheckInterval {
		t.Errorf("operator CheckInterval default = %s, want shared %s", cfg.CheckInterval, liveness.Default().AlertCheckInterval)
	}
}

// TestApplyOperatorDefaultsCheckIntervalOverrideFlows verifies an explicit
// operator.check_interval wins over the liveness-derived default.
func TestApplyOperatorDefaultsCheckIntervalOverrideFlows(t *testing.T) {
	cfg := OperatorConfig{CheckInterval: 11 * time.Second}
	applyOperatorDefaults(&cfg, true)
	if cfg.CheckInterval != 11*time.Second {
		t.Fatalf("explicit operator check_interval must win: got %s, want 11s", cfg.CheckInterval)
	}
}
