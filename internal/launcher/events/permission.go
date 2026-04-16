package events

const KindPermission EventKind = "permission"

type PermissionPayload struct {
	Agent     string                     `json:"agent"`
	Role      string                     `json:"role"`
	GitHub    PermissionGitHubPayload    `json:"github"`
	FS        PermissionFSPayload        `json:"fs"`
	Resources PermissionResourcesPayload `json:"resources"`
}

type PermissionGitHubPayload struct {
	Token  string `json:"token"`
	Source string `json:"source"`
}

type PermissionFSPayload struct {
	Write string `json:"write"`
}

type PermissionResourcesPayload struct {
	MaxMemoryMB   int `json:"max_memory_mb"`
	MaxCPUPercent int `json:"max_cpu_percent"`
}
