package app

import (
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestResolveAgentConfigPerRepo verifies the coordinator resolves agent
// config from the PER-REPO registration, so two repos sharing one worker but
// declaring different dev_container_image / runtime for the same agent name
// yield distinct dispatched Agent configs (ADR 2026-06-06 §2).
func TestResolveAgentConfigPerRepo(t *testing.T) {
	pm := &PollerManager{
		runtimes: map[string]*RepoRuntime{
			"owner/repo-a": {Config: &config.FullConfig{Agents: map[string]*config.AgentConfig{
				"dev": {Name: "dev", Runtime: "claude-code", DevContainerImage: "a/dev:1"},
			}}},
			"owner/repo-b": {Config: &config.FullConfig{Agents: map[string]*config.AgentConfig{
				"dev": {Name: "dev", Runtime: "agentm", DevContainerImage: "b/dev:2"},
			}}},
		},
	}

	a := pm.ResolveAgentConfig("owner/repo-a", "dev")
	b := pm.ResolveAgentConfig("owner/repo-b", "dev")
	if a == nil || b == nil {
		t.Fatalf("expected per-repo configs, got a=%v b=%v", a, b)
	}
	if a.DevContainerImage != "a/dev:1" || a.Runtime != "claude-code" {
		t.Fatalf("repo-a config wrong: %+v", a)
	}
	if b.DevContainerImage != "b/dev:2" || b.Runtime != "agentm" {
		t.Fatalf("repo-b config wrong: %+v", b)
	}
}

// TestResolveAgentConfigReturnsCopy verifies callers get a DEEP copy: mutating
// reference-typed fields (slices, nested maps) on the returned config must not
// leak into the live registration. A shallow copy would share the underlying
// slice/map arrays and fail these assertions.
func TestResolveAgentConfigReturnsCopy(t *testing.T) {
	live := &config.AgentConfig{
		Name:              "dev",
		DevContainerImage: "orig:1",
		Context:           []string{"ctx-a"},
		Extensions: []config.AgentExtension{
			{Module: "mod", Config: map[string]any{"x": 1}},
		},
	}
	pm := &PollerManager{
		runtimes: map[string]*RepoRuntime{
			"owner/repo": {Config: &config.FullConfig{Agents: map[string]*config.AgentConfig{"dev": live}}},
		},
	}

	got := pm.ResolveAgentConfig("owner/repo", "dev")
	if got == live {
		t.Fatal("expected a copy, got the live pointer")
	}

	// Mutate a scalar.
	got.DevContainerImage = "mutated:9"
	// Mutate reference-typed fields: append to a slice and rewrite a nested map.
	got.Context = append(got.Context, "ctx-b")
	got.Context[0] = "ctx-mutated"
	got.Extensions[0].Config["x"] = 99
	got.Extensions[0].Module = "mutated-mod"

	if live.DevContainerImage != "orig:1" {
		t.Fatalf("scalar mutation leaked into live config: %q", live.DevContainerImage)
	}
	if len(live.Context) != 1 || live.Context[0] != "ctx-a" {
		t.Fatalf("Context slice mutation leaked into live config: %v", live.Context)
	}
	if live.Extensions[0].Config["x"] != 1 {
		t.Fatalf("Extensions nested map mutation leaked into live config: %v", live.Extensions[0].Config)
	}
	if live.Extensions[0].Module != "mod" {
		t.Fatalf("Extensions slice mutation leaked into live config: %q", live.Extensions[0].Module)
	}
}

// TestResolveAgentConfigMissing returns nil for unknown repo or agent so the
// worker falls back to its local config.
func TestResolveAgentConfigMissing(t *testing.T) {
	pm := &PollerManager{
		runtimes: map[string]*RepoRuntime{
			"owner/repo": {Config: &config.FullConfig{Agents: map[string]*config.AgentConfig{
				"dev": {Name: "dev"},
			}}},
		},
	}

	if got := pm.ResolveAgentConfig("owner/unknown", "dev"); got != nil {
		t.Fatalf("expected nil for unknown repo, got %+v", got)
	}
	if got := pm.ResolveAgentConfig("owner/repo", "ghost"); got != nil {
		t.Fatalf("expected nil for unknown agent, got %+v", got)
	}
}

// TestClaimNextTaskPopulatesAgentFromRegistration covers the dispatch seam
// end-to-end: a claimed task for a registered repo carries the per-repo Agent
// config in TaskPollResponse, resolved via ResolveAgentConfig. This is the one
// line in claimNextTask that, if dropped, makes the wire-config feature a
// silent no-op while every isolated unit test still passes (ADR 2026-06-06 §2).
func TestClaimNextTaskPopulatesAgentFromRegistration(t *testing.T) {
	st := newCoordinatorTestStore(t)

	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-seam",
		Repo:      "owner/repo",
		IssueNum:  7,
		AgentName: "dev",
		Role:      "dev",
		Runtime:   "agentm",
		Workflow:  "default",
		State:     "developing",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	pm := &PollerManager{
		runtimes: map[string]*RepoRuntime{
			"owner/repo": {Config: &config.FullConfig{Agents: map[string]*config.AgentConfig{
				"dev": {
					Name:              "dev",
					Runtime:           "agentm",
					DevContainerImage: "owner/dev:9",
					Prompt:            "do the work",
				},
			}}},
		},
	}

	server := &FullCoordinatorServer{
		Store:    st,
		Eventlog: eventlog.NewEventLogger(st),
		Pollers:  pm,
	}

	worker := &store.WorkerRecord{
		ID:        "worker-a",
		Repo:      "owner/repo",
		ReposJSON: `["owner/repo"]`,
		Roles:     `["dev"]`,
		Runtime:   "agentm",
	}

	resp, err := server.claimNextTask(worker)
	if err != nil {
		t.Fatalf("claimNextTask: %v", err)
	}
	if resp == nil {
		t.Fatal("expected a claimed task, got nil")
	}
	if resp.TaskID != "task-seam" || resp.AgentName != "dev" {
		t.Fatalf("unexpected claimed task: %+v", resp)
	}
	if resp.Agent == nil {
		t.Fatal("dispatch seam dropped Agent: TaskPollResponse.Agent is nil")
	}
	if resp.Agent.DevContainerImage != "owner/dev:9" || resp.Agent.Prompt != "do the work" {
		t.Fatalf("Agent not resolved from per-repo registration: %+v", resp.Agent)
	}
}
