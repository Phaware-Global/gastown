package tmux

import (
	"testing"

	"github.com/steveyegge/gastown/internal/tmuxguard"
)

// Every test binary that links this package is cut off from the tmux server it
// was started under, before any of its code runs. Doing it here means no test
// package can forget to.
func init() {
	if testing.Testing() {
		tmuxguard.Isolate()
	}
}
