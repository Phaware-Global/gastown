package daemon

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
)

// bdReadOnlyEnv returns an environment slice for read-only bd/gt subprocess
// calls invoked by the daemon. It forces BD_DOLT_AUTO_COMMIT=off so that
// read-only operations (status checks, list, show) do not request a Dolt
// auto-commit on completion. Without this, every read-only call opens a
// fresh connection to attempt a no-op commit, producing thousands of
// failed-but-counted connections per hour on idle towns and spamming
// dolt.log. See gh#3596.
//
// Existing BD_DOLT_AUTO_COMMIT entries are filtered out before appending
// the authoritative "off" value, because glibc getenv() returns the first
// matching entry — a stale "on" earlier in the slice would otherwise win.
func bdReadOnlyEnv() []string {
	return beads.BuildReadOnlyRoutingBDEnv(os.Environ(), "")
}

func bdReadOnlyRoutingEnv(townRoot string) []string {
	fallback := ""
	if townRoot != "" {
		fallback = filepath.Join(townRoot, ".beads")
	}
	return beads.BuildReadOnlyRoutingBDEnv(os.Environ(), fallback)
}

func bdMutationRoutingEnv(townRoot string) []string {
	fallback := ""
	if townRoot != "" {
		fallback = filepath.Join(townRoot, ".beads")
	}
	return beads.BuildMutationRoutingBDEnv(os.Environ(), fallback)
}

func bdReadOnlyPinnedEnv(beadsDir string) []string {
	return beads.BuildReadOnlyPinnedBDEnv(os.Environ(), beadsDir)
}
