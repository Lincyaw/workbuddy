package runtime

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

const (
	tokenSourceNone   = "none"
	tokenSourceScoped = "scoped"
	tokenSourceHost   = "host"
	defaultFSWrite    = "unrestricted"
)

var hostTokenKeys = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GITHUB_OAUTH",
}

func BuildEnvVars(task *TaskContext) []string {
	return []string{
		"WORKBUDDY_ISSUE_NUMBER=" + strconv.Itoa(task.Issue.Number),
		"WORKBUDDY_ISSUE_TITLE=" + task.Issue.Title,
		"WORKBUDDY_ISSUE_BODY=" + task.Issue.Body,
		"WORKBUDDY_REPO=" + task.Repo,
		"WORKBUDDY_SESSION_ID=" + task.Session.ID,
	}
}

func BuildScopedEnv(agent *config.AgentConfig, task *TaskContext) []string {
	var agentName, agentRole string
	var permissions config.PermissionsConfig
	if agent != nil {
		agentName = agent.Name
		agentRole = agent.Role
		permissions = agent.Permissions
	}

	scopedTokenEnv := strings.TrimSpace(permissions.GitHub.Token)
	envCapacity := len(os.Environ()) + 4
	if task != nil {
		envCapacity += len(BuildEnvVars(task))
	}
	env := make([]string, 0, envCapacity)
	for _, entry := range os.Environ() {
		key := envKey(entry)
		if isHostGitHubTokenKey(key) {
			continue
		}
		env = append(env, entry)
	}
	if task != nil {
		env = append(env, BuildEnvVars(task)...)
	}

	if scopedTokenEnv != "" {
		tokenValue, ok := os.LookupEnv(scopedTokenEnv)
		if ok && strings.TrimSpace(tokenValue) != "" {
			env = append(env, "GH_TOKEN="+tokenValue)
			log.Printf("runtime: using scoped GitHub token env %q for agent %q role %q", scopedTokenEnv, agentName, agentRole)
			return env
		}
		log.Printf("runtime: permissions.github.token %q is not set for agent %q role %q, falling back to host token", scopedTokenEnv, agentName, agentRole)
	}

	hostTokenEnv, hostTokenValue, ok := findHostGitHubToken()
	if ok && hostTokenValue != "" {
		env = append(env, "GH_TOKEN="+hostTokenValue)
		log.Printf("runtime: using host GitHub token env %q for agent %q role %q", hostTokenEnv, agentName, agentRole)
		return env
	}

	if token, err := ghAuthToken(); err == nil && token != "" {
		env = append(env, "GH_TOKEN="+token)
		log.Printf("runtime: using gh-cli keyring token for agent %q role %q", agentName, agentRole)
		return env
	}

	log.Printf("runtime: no scoped or host GitHub token available for agent %q role %q", agentName, agentRole)
	return env
}

func EmitPermissionEvent(events chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, agent *config.AgentConfig, emit func(chan<- launcherevents.Event, *uint64, string, string, launcherevents.EventKind, any, []byte)) {
	if events == nil {
		return
	}
	payload := EffectivePermissionsPayload(agent)
	emit(events, seq, sessionID, turnID, launcherevents.KindPermission, payload, nil)
}

func EffectivePermissionsPayload(agent *config.AgentConfig) launcherevents.PermissionPayload {
	var tokenEnv, tokenSource string
	var fsWrite string
	var resources launcherevents.PermissionResourcesPayload
	fsWrite = defaultFSWrite
	if agent != nil {
		fsWrite = strings.TrimSpace(agent.Permissions.FS.Write)
	}
	if fsWrite == "" {
		fsWrite = defaultFSWrite
	}

	role := ""
	name := ""
	configured := ""
	if agent != nil {
		configured = strings.TrimSpace(agent.Permissions.GitHub.Token)
		name = agent.Name
		role = agent.Role
	}

	if configured != "" {
		tokenEnv = configured
		tokenSource = tokenSourceScoped
		tokenValue, ok := os.LookupEnv(configured)
		if !ok || strings.TrimSpace(tokenValue) == "" {
			tokenSource = tokenSourceHost
			tokenEnv, tokenSource = tokenFromHost()
		}
	} else {
		tokenEnv, tokenSource = tokenFromHost()
	}

	if tokenEnv == "" {
		tokenSource = tokenSourceNone
	}

	if agent != nil {
		resources = launcherevents.PermissionResourcesPayload{
			MaxMemoryMB:   agent.Permissions.Resources.MaxMemoryMB,
			MaxCPUPercent: agent.Permissions.Resources.MaxCPUPercent,
		}
	}

	return launcherevents.PermissionPayload{
		Agent: name,
		Role:  role,
		GitHub: launcherevents.PermissionGitHubPayload{
			Token:  tokenEnv,
			Source: tokenSource,
		},
		FS: launcherevents.PermissionFSPayload{
			Write: fsWrite,
		},
		Resources: resources,
	}
}

func tokenFromHost() (string, string) {
	hostEnv, hostValue, ok := findHostGitHubToken()
	if ok && strings.TrimSpace(hostValue) != "" {
		return hostEnv, tokenSourceHost
	}
	if token, err := ghAuthToken(); err == nil && token != "" {
		return "gh-cli-keyring", tokenSourceHost
	}
	return "", tokenSourceNone
}

func findHostGitHubToken() (env, value string, ok bool) {
	for _, key := range hostTokenKeys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return key, value, true
		}
	}
	return "", "", false
}

func isHostGitHubTokenKey(key string) bool {
	for _, candidate := range hostTokenKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

func envKey(entry string) string {
	parts := strings.SplitN(entry, "=", 2)
	return parts[0]
}

func ghAuthToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
