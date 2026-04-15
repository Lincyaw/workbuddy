package events

const KindFileChange EventKind = "file.change"

type FileChangePayload struct {
	Path       string `json:"path"`
	ChangeKind string `json:"change_kind"`
	Diff       string `json:"diff,omitempty"`
}
