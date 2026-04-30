// Package supervisor implements the local agent-process supervisor.
//
// The supervisor owns long-lived agent subprocesses on behalf of (potentially
// transient) workbuddy worker processes. It exposes a small HTTP API over a
// unix socket so callers on the same host can start, observe, and cancel
// agent runs without keeping the worker pid alive.
package supervisor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readProcStarttime parses field 22 (starttime) from /proc/<pid>/stat. The
// value is the time the process started after system boot, expressed in clock
// ticks. Combined with pid it is a stable per-process identifier that lets
// the supervisor detect pid reuse after a restart.
func readProcStarttime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The comm field (2) is wrapped in parentheses and may contain spaces or
	// other delimiters, so locate the trailing ')' before splitting.
	s := string(data)
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 || rp+2 >= len(s) {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	rest := strings.Fields(s[rp+2:])
	// rest[0] is field 3 (state). starttime is field 22, so index 19 here.
	if len(rest) < 20 {
		return 0, fmt.Errorf("/proc/%d/stat has %d fields, want >= 22", pid, len(rest)+2)
	}
	return strconv.ParseUint(rest[19], 10, 64)
}
