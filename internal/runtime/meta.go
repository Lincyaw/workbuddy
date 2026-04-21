package runtime

import (
	"encoding/json"
	"strings"
)

const (
	metaBeginMarker = "WORKBUDDY_META_BEGIN"
	metaEndMarker   = "WORKBUDDY_META_END"
)

// ParseMeta extracts the WORKBUDDY_META JSON block from stdout.
// Returns nil (not an error) if the block is missing or malformed.
func ParseMeta(stdout string) map[string]string {
	beginIdx := strings.Index(stdout, metaBeginMarker)
	if beginIdx < 0 {
		return nil
	}
	after := stdout[beginIdx+len(metaBeginMarker):]

	endIdx := strings.Index(after, metaEndMarker)
	if endIdx < 0 {
		return nil
	}

	jsonStr := strings.TrimSpace(after[:endIdx])
	if jsonStr == "" {
		return nil
	}

	var meta map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
		return nil
	}

	return meta
}
