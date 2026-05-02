package launcher

import (
	"testing"

	"github.com/Lincyaw/workbuddy/internal/agent/codex/codextest"
)

// installFakeCodex starts an in-process codextest WebSocket fake and points
// the codex backend at it via $WORKBUDDY_CODEX_URL. Returns the cleanup
// function (matching the legacy installFakeCodex signature so callers
// don't all have to change). Replaces the pre-REQ-127 PATH-based python
// stdio fake.
func installFakeCodex(t *testing.T) func() {
	t.Helper()
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeComplete})
	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)
	return func() {
		srv.Close()
	}
}
