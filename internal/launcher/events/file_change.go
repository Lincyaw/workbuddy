package events

type FileChangePayload struct {
	Path       string `json:"path"`
	ChangeKind string `json:"change_kind"`
	Diff       string `json:"diff,omitempty"`
}
