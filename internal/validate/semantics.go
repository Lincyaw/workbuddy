package validate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"gopkg.in/yaml.v3"
)

// Diagnostic codes emitted by the Layer-4 semantic checks.
const (
	// CodeTimeoutBelowStaleThreshold — agent.policy.timeout is shorter
	// than worker.stale_inference.idle_threshold, so the watchdog
	// would kill the agent before its own timeout fires.
	CodeTimeoutBelowStaleThreshold = "WB-S001"

	// CodeTimeoutSuspiciouslyLarge — `policy.timeout` exceeds 4h.
	// Almost always a unit typo (`60h` vs `60m`).
	CodeTimeoutSuspiciouslyLarge = "WB-S002"

	// CodeRuntimeBinaryMissing — the runtime binary is not on $PATH.
	// Suppressible via `--no-runtime-check` for CI/sandbox contexts.
	CodeRuntimeBinaryMissing = "WB-S003"

	// CodeAgentInTerminalState — an agent is wired to a workflow state
	// that has no outgoing transitions. Terminal states should not
	// invoke an agent because there is no path forward.
	CodeAgentInTerminalState = "WB-S004"

	// CodeRuntimeNoWorkerSupport — an agent declares a runtime that no local
	// registered worker currently advertises, so dispatch would dead-end.
	CodeRuntimeNoWorkerSupport = "WB-S005"
)

// suspiciousTimeoutCutoff is the upper bound past which WB-S002 fires.
// 4h is a reasonable ceiling for the realistic agent-run budget; the
// next plausible duration (60h) is almost certainly a typo.
const suspiciousTimeoutCutoff = 4 * time.Hour

// runtimeBinaries maps a declared runtime to the binary name we expect
// on $PATH. Multiple runtimes may share a binary (claude-code and
// claude-agent-sdk both resolve via the `claude` CLI).
var runtimeBinaries = map[string]string{
	"codex":            "codex",
	"claude-code":      "claude",
	"claude-agent-sdk": "claude",
}

type semanticsOptions struct {
	SkipRuntimeBinaryCheck bool
	WorkerRuntimes         map[string]struct{}
	CheckWorkerRuntimes    bool
}

// validateSemantics implements WB-S001..WB-S004.
//
// It rereads `config.yaml` independently of the rest of the validator so
// that a malformed config doesn't tank earlier (cheaper) layers — a
// missing/bad config simply causes WB-S001 to skip its check.
func validateSemantics(
	configDir string,
	agents map[string]*agentDoc,
	workflows []*workflowDoc,
	opts semanticsOptions,
) []Diagnostic {
	var diags []Diagnostic

	idleThreshold := loadStaleIdleThreshold(filepath.Join(configDir, "config.yaml"))

	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		agent := agents[name]
		diags = append(diags, validateAgentSemantics(agent, idleThreshold, opts)...)
	}

	// WB-S004 — terminal-state-with-agent. A "terminal" state is one
	// with no outgoing transitions; running an agent there is a
	// dead-end (the agent's label flip never gets routed onward).
	for _, wf := range workflows {
		for _, stateName := range wf.StateOrder {
			state := wf.States[stateName]
			if state == nil {
				continue
			}
			agentName := strings.TrimSpace(state.Agent)
			if agentName == "" {
				continue
			}
			if len(state.Transitions) > 0 {
				continue
			}
			// Don't flag the well-known terminal `failed` state —
			// it intentionally has no agent, but if someone adds
			// one we still flag it for review.
			diags = append(diags, Diagnostic{
				Path:     wf.Path,
				Line:     orFallback(state.AgentLine, state.Line),
				Severity: SeverityError,
				Code:     CodeAgentInTerminalState,
				Message: fmt.Sprintf(
					"workflow %q state %q is terminal (no transitions) but assigns agent %q",
					wf.Name, stateName, agentName,
				),
			})
		}
	}
	return diags
}

func validateAgentSemantics(agent *agentDoc, idleThreshold time.Duration, opts semanticsOptions) []Diagnostic {
	if agent == nil {
		return nil
	}
	var diags []Diagnostic

	// WB-S001 — timeout < idle_threshold.
	if agent.PolicyTimeout > 0 && idleThreshold > 0 && agent.PolicyTimeout < idleThreshold {
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     orFallback(agent.PolicyTimeoutLine, agent.NameLine),
			Severity: SeverityError,
			Code:     CodeTimeoutBelowStaleThreshold,
			Message: fmt.Sprintf(
				"agent %q policy.timeout (%s) is shorter than worker.stale_inference.idle_threshold (%s); the watchdog will kill the agent before its own timeout fires",
				agent.Name, agent.PolicyTimeout, idleThreshold,
			),
		})
	}

	// WB-S002 — suspiciously large timeout.
	if agent.PolicyTimeout > suspiciousTimeoutCutoff {
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     orFallback(agent.PolicyTimeoutLine, agent.NameLine),
			Severity: SeverityWarning,
			Code:     CodeTimeoutSuspiciouslyLarge,
			Message: fmt.Sprintf(
				"agent %q policy.timeout (%s) exceeds %s; double-check the unit (e.g. 60m vs 60h)",
				agent.Name, agent.PolicyTimeout, suspiciousTimeoutCutoff,
			),
		})
	}

	// WB-S003 — runtime binary missing on $PATH.
	if !opts.SkipRuntimeBinaryCheck {
		if rt := strings.TrimSpace(agent.Runtime); rt != "" {
			if bin, ok := runtimeBinaries[rt]; ok {
				if _, err := exec.LookPath(bin); err != nil {
					diags = append(diags, Diagnostic{
						Path:     agent.Path,
						Line:     orFallback(agent.RuntimeLine, agent.NameLine),
						Severity: SeverityWarning,
						Code:     CodeRuntimeBinaryMissing,
						Message: fmt.Sprintf(
							"agent %q declares runtime %q but binary %q was not found on $PATH (suppress with --no-runtime-check)",
							agent.Name, rt, bin,
						),
					})
				}
			}
		}
	}

	// WB-S005 — local workers do not advertise the agent's explicit runtime.
	if opts.CheckWorkerRuntimes {
		if rt := strings.TrimSpace(agent.Runtime); rt != "" {
			if _, ok := opts.WorkerRuntimes[rt]; !ok {
				advertised := make([]string, 0, len(opts.WorkerRuntimes))
				for runtime := range opts.WorkerRuntimes {
					advertised = append(advertised, runtime)
				}
				sort.Strings(advertised)
				msg := "no registered workers advertise any runtime"
				if len(advertised) > 0 {
					msg = fmt.Sprintf("registered worker runtimes: %s", strings.Join(advertised, ", "))
				}
				diags = append(diags, Diagnostic{
					Path:     agent.Path,
					Line:     orFallback(agent.RuntimeLine, agent.NameLine),
					Severity: SeverityWarning,
					Code:     CodeRuntimeNoWorkerSupport,
					Message: fmt.Sprintf(
						"agent %q declares runtime %q but no registered worker advertises it (%s)",
						agent.Name, rt, msg,
					),
				})
			}
		}
	}

	return diags
}

// loadStaleIdleThreshold reads worker.stale_inference.idle_threshold from
// the config file. Returns 0 (and skips WB-S001) on any failure.
func loadStaleIdleThreshold(path string) time.Duration {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var cfg struct {
		Worker config.WorkerConfig `yaml:"worker"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return 0
	}
	return cfg.Worker.StaleInference.IdleThreshold
}
