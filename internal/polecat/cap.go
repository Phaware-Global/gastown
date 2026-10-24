package polecat

import (
	"fmt"
	"os"
	"strings"
)

// MinPolecatDirsPerRig is the floor for the per-rig polecat directory cap. A rig's
// effective cap is max(its configured max_polecats, MinPolecatDirsPerRig). Fresh
// polecat allocation is refused once a rig's polecat-directory count reaches the
// cap, so stale directories must be reclaimed to keep spawns from deadlocking.
const MinPolecatDirsPerRig = 30

// EffectivePolecatDirCap returns the per-rig polecat directory cap: the configured
// max_polecats floored at MinPolecatDirsPerRig.
func EffectivePolecatDirCap(configured int) int {
	if configured < MinPolecatDirsPerRig {
		return MinPolecatDirsPerRig
	}
	return configured
}

// CountPolecatDirs returns how many polecat directories a rig holds under
// polecatsDir. This is the number the cap is enforced against, so the spawn path,
// the daemon's cap-relief reaper and "gt polecat list" all count the same way:
// every non-hidden subdirectory, whether or not it has a live session.
func CountPolecatDirs(polecatsDir string) (int, error) {
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n, nil
}

// CheckDirCap returns an error naming the cap when a rig already holds cap or more
// polecat directories, so a fresh allocation fails loudly instead of doing nothing.
// A missing polecats directory counts as zero.
func CheckDirCap(rigName, polecatsDir string, cap int) error {
	n, err := CountPolecatDirs(polecatsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("counting polecat directories for rig %s: %w", rigName, err)
	}
	if n >= cap {
		return fmt.Errorf("rig %s has %d polecat directories (max %d): no slot free for a new polecat. "+
			"Reclaim stale ones with 'gt polecat stale %s --cleanup', or resolve recovery-needed polecats: gt polecat list %s",
			rigName, n, cap, rigName, rigName)
	}
	return nil
}
