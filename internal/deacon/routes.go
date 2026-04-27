package deacon

import (
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
)

// SyncBeadsRoutes writes cross-database routing rules to the deacon's .beads/
// directory so that bd commands run from the deacon session can resolve beads
// in the town-level HQ database (e.g., bd show hq-wisp-xxx).
//
// Without this file, bd auto-discovers the deacon's own .beads/ directory and
// has no route table pointing to HQ, so hq-* lookups fail with "not found"
// even though the bead exists in the HQ database.
//
// The routes are derived from the town-level routes.jsonl: paths that are town-
// level (".") become ".." (parent of the deacon dir = town root), and rig-
// relative paths become "../<path>". This is called on every deacon Start() so
// new rigs added to the town are automatically visible from the deacon session.
func SyncBeadsRoutes(townRoot, deaconDir string) error {
	hqBeadsDir := filepath.Join(townRoot, ".beads")
	hqRoutes, err := beads.LoadRoutes(hqBeadsDir)
	if err != nil {
		return err
	}
	if len(hqRoutes) == 0 {
		return nil
	}

	deaconBeadsDir := filepath.Join(deaconDir, ".beads")

	// Transform HQ routes to paths relative to the deacon directory.
	// ResolveBeadsDirForID computes: filepath.Join(filepath.Dir(beadsDir), route.Path),
	// so route paths must be relative to filepath.Dir(deaconBeadsDir) = deaconDir.
	var deaconRoutes []beads.Route
	for _, r := range hqRoutes {
		var relativePath string
		if r.Path == "." {
			// Town-level bead: deaconDir/.. == townRoot
			relativePath = ".."
		} else {
			// Rig-level bead: deaconDir/../<rigPath> == townRoot/<rigPath>
			relativePath = "../" + r.Path
		}
		deaconRoutes = append(deaconRoutes, beads.Route{
			Prefix: r.Prefix,
			Path:   relativePath,
		})
	}

	return beads.WriteRoutes(deaconBeadsDir, deaconRoutes)
}
