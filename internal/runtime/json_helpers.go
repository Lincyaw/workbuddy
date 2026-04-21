package runtime

import (
	"encoding/json"
	"strings"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func rawString(raw map[string]json.RawMessage, key string) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw[key], &s); err == nil {
		return s
	}
	return ""
}

func rawStringSlice(raw map[string]json.RawMessage, key string) []string {
	if raw == nil {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw[key], &values); err == nil {
		return values
	}
	var single string
	if err := json.Unmarshal(raw[key], &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

func rawInt(raw map[string]json.RawMessage, key string) (int, bool) {
	if raw == nil {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return 0, false
	}
	return value, true
}

func rawIntValue(raw map[string]json.RawMessage, key string) int {
	value, _ := rawInt(raw, key)
	return value
}

func rawBool(raw map[string]json.RawMessage, key string) bool {
	if raw == nil {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return false
	}
	return value
}

func rawObject(raw map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return nil
	}
	return value
}

func cloneRaw(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

func rawPatchChanges(raw map[string]json.RawMessage) []launcherevents.FileChangePayload {
	diff := firstNonEmpty(rawString(raw, "patch"), rawString(raw, "diff"))
	if diff != "" {
		if changes := splitPatch(diff); len(changes) > 0 {
			return changes
		}
	}

	var files []struct {
		Path       string `json:"path"`
		ChangeKind string `json:"change_kind"`
	}
	if err := json.Unmarshal(raw["files"], &files); err == nil && len(files) > 0 {
		out := make([]launcherevents.FileChangePayload, 0, len(files))
		for _, file := range files {
			changeKind := file.ChangeKind
			if changeKind == "" {
				changeKind = "modify"
			}
			out = append(out, launcherevents.FileChangePayload{Path: file.Path, ChangeKind: changeKind, Diff: diff})
		}
		return out
	}

	paths := rawStringSlice(raw, "paths")
	if len(paths) == 0 {
		if path := rawString(raw, "path"); path != "" {
			paths = []string{path}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	out := make([]launcherevents.FileChangePayload, 0, len(paths))
	for _, path := range paths {
		out = append(out, launcherevents.FileChangePayload{Path: path, ChangeKind: "modify", Diff: diff})
	}
	return out
}

func splitPatch(diff string) []launcherevents.FileChangePayload {
	diff = strings.TrimSpace(diff)
	if diff == "" {
		return nil
	}

	var out []launcherevents.FileChangePayload
	sections := strings.Split(diff, "diff --git ")
	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		patch := "diff --git " + section
		lines := strings.Split(section, "\n")
		path := ""
		changeKind := "modify"
		if len(lines) > 0 {
			fields := strings.Fields(lines[0])
			if len(fields) >= 2 {
				path = strings.TrimPrefix(fields[1], "b/")
				if path == fields[1] {
					path = strings.TrimPrefix(fields[len(fields)-1], "b/")
				}
			}
		}
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line, "new file mode"):
				changeKind = "create"
			case strings.HasPrefix(line, "deleted file mode"):
				changeKind = "delete"
			case strings.HasPrefix(line, "+++ b/"):
				path = strings.TrimPrefix(line, "+++ b/")
			case strings.HasPrefix(line, "--- /dev/null"):
				changeKind = "create"
			case strings.HasPrefix(line, "+++ /dev/null"):
				changeKind = "delete"
			}
		}
		if path == "" {
			continue
		}
		out = append(out, launcherevents.FileChangePayload{Path: path, ChangeKind: changeKind, Diff: patch})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
