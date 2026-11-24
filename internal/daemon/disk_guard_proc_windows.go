//go:build windows

package daemon

import (
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// setEscalationSession has no session concept on Windows; it falls back to
// the shared process-group helper.
func setEscalationSession(cmd *exec.Cmd, grace time.Duration) {
	util.SetGracefulProcessGroup(cmd, grace)
}
