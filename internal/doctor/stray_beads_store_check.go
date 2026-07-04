package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// StrayBeadsStoreCheck detects stray embedded beads stores — a `.beads`
// directory holding an `embeddeddolt` database at a non-canonical location.
//
// These appear when a beads dir is resolved from an empty town root
// (producing a relative path like "heartworks_android/.beads" that resolves
// against the subprocess cwd) and bd then silently bootstraps an embedded
// Dolt store at the bogus path. Writes land in the stray store and are
// invisible to the shared server — the "silent write loss" incident of
// 2026-07-04 (hq-09sb1 follow-up), where escalation beads vanished into
// <rig>/refinery/rig/<rigname>/.beads/embeddeddolt.
//
// Fix quarantines the stray store (rename to .beads.quarantined-<date>) and
// plants a redirect file at the stray location pointing to the enclosing
// canonical store, so a recurrence of the misroute lands in the right
// database instead of a fresh embedded one. Quarantined data is preserved
// for manual recovery.
type StrayBeadsStoreCheck struct {
	FixableCheck
	strays []string // absolute paths of stray .beads dirs, cached for Fix
}

// NewStrayBeadsStoreCheck creates a new stray beads store check.
func NewStrayBeadsStoreCheck() *StrayBeadsStoreCheck {
	return &StrayBeadsStoreCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stray-beads-store",
				CheckDescription: "Check for stray embedded beads stores that strand writes",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run scans plausible worker directories for stray embedded stores.
func (c *StrayBeadsStoreCheck) Run(ctx *CheckContext) *CheckResult {
	c.strays = findStrayBeadsStores(ctx.TownRoot)

	if len(c.strays) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No stray embedded beads stores",
		}
	}

	details := make([]string, 0, len(c.strays))
	for _, s := range c.strays {
		details = append(details, s)
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d stray embedded beads store(s) capturing writes", len(c.strays)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to quarantine strays and plant redirects; recover any trapped beads from the quarantined stores",
	}
}

// Fix quarantines each stray store and plants a redirect to the enclosing
// canonical store. Trapped data is preserved in the quarantined directory.
func (c *StrayBeadsStoreCheck) Fix(ctx *CheckContext) error {
	if len(c.strays) == 0 {
		c.strays = findStrayBeadsStores(ctx.TownRoot)
	}

	stamp := time.Now().Format("20060102-150405")
	var firstErr error
	for _, stray := range c.strays {
		quarantined := stray + ".quarantined-" + stamp
		if err := os.Rename(stray, quarantined); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("quarantine %s: %w", stray, err)
			}
			continue
		}

		// Plant a redirect so a recurring misroute lands in the canonical
		// store. Skip when the stray nests directly inside a canonical store
		// (.beads/.beads) — nothing should resolve a workspace there, and a
		// redirect would point a store at itself.
		parent := filepath.Dir(stray)
		if filepath.Base(parent) == ".beads" {
			continue
		}
		target := enclosingCanonicalStore(ctx.TownRoot, parent)
		if target == "" {
			continue
		}
		rel, err := filepath.Rel(parent, target)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(stray, 0o755); err != nil {
			continue
		}
		// Best-effort: the quarantine already removed the hazard.
		_ = os.WriteFile(filepath.Join(stray, "redirect"), []byte(rel), 0o644)
	}
	return firstErr
}

// findStrayBeadsStores probes the misroute signature — <workerDir>/<rigName
// or prefix>/.beads/embeddeddolt — across the town's worker directories,
// plus the nested <store>/.beads/.beads variant.
func findStrayBeadsStores(townRoot string) []string {
	if townRoot == "" {
		return nil
	}

	rigDirs, err := findRigDirs(townRoot)
	if err != nil {
		return nil
	}

	// Names a misrouted join can produce: rig directory names, route path
	// heads, and bead prefixes (with and without the trailing dash — the
	// deacon incident produced a directory literally named "gt-").
	// Route-derived values come verbatim from routes.jsonl, so reject
	// anything empty, degenerate, or carrying path separators/traversal:
	// a hostile or malformed route must not make the probe collapse onto a
	// base's own .beads (empty name) or escape the town tree ("../..").
	nameSet := map[string]bool{}
	addName := func(name string) {
		if name == "" || name == "." || name == ".." {
			return
		}
		if strings.ContainsAny(name, `/\`) {
			return
		}
		nameSet[name] = true
	}
	for _, rigDir := range rigDirs {
		addName(filepath.Base(rigDir))
	}
	if routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads")); err == nil {
		for _, r := range routes {
			if r.Prefix != "" {
				addName(r.Prefix)
				addName(strings.TrimSuffix(r.Prefix, "-"))
			}
			if r.Path != "" && r.Path != "." {
				head := r.Path
				if i := strings.IndexByte(head, '/'); i > 0 {
					head = head[:i]
				}
				addName(head)
			}
		}
	}

	// Worker directories whose cwd a misrouted bd call can inherit.
	bases := []string{filepath.Join(townRoot, "deacon")}
	for _, rigDir := range rigDirs {
		bases = append(bases, rigDir)
		for _, worker := range []string{"witness", "refinery", "mayor", "reviewer"} {
			bases = append(bases, filepath.Join(rigDir, worker))
		}
		bases = append(bases, getWorktreePaths(rigDir)...)
	}

	seen := map[string]bool{}
	var strays []string
	addStray := func(beadsDir string) {
		if seen[beadsDir] {
			return
		}
		if dirExists(filepath.Join(beadsDir, "embeddeddolt")) {
			seen[beadsDir] = true
			strays = append(strays, beadsDir)
		}
	}

	for _, base := range bases {
		if !dirExists(base) {
			continue
		}
		for name := range nameSet {
			addStray(filepath.Join(base, name, ".beads"))
		}
		// Nested-inside-a-store variant: <base>/.beads/.beads.
		addStray(filepath.Join(base, ".beads", ".beads"))
	}
	// Town-store nested variant.
	addStray(filepath.Join(townRoot, ".beads", ".beads"))

	return strays
}

// enclosingCanonicalStore walks up from dir (staying inside townRoot) and
// returns the first sibling .beads that resolves — through any redirect
// pointers — to a real store; that is where misrouted writes should have
// gone. Candidates whose redirect chain is broken or too deep are skipped
// so a planted redirect never chains through another redirect or points at
// a store that does not exist.
func enclosingCanonicalStore(townRoot, dir string) string {
	townRoot = filepath.Clean(townRoot)
	dir = filepath.Clean(dir)
	for {
		candidate := filepath.Join(dir, ".beads")
		if isCanonicalStore(candidate) {
			if resolved := followRedirects(candidate); resolved != "" && isCanonicalStore(resolved) {
				return resolved
			}
		}
		if dir == townRoot {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir || (parent != townRoot && !strings.HasPrefix(parent, townRoot+string(filepath.Separator))) {
			return ""
		}
		dir = parent
	}
}

// followRedirects resolves a chain of .beads redirect pointers (bounded) to
// the store they ultimately name. Redirect content is relative to the .beads
// directory's parent, matching gt's redirect convention. Returns "" when the
// chain is still redirecting at the depth cap, so callers never treat an
// unresolved pointer as a final store.
func followRedirects(beadsDir string) string {
	for i := 0; i < 8; i++ {
		data, err := os.ReadFile(filepath.Join(beadsDir, "redirect"))
		if err != nil {
			return beadsDir
		}
		target := strings.TrimSpace(string(data))
		if target == "" {
			// An empty redirect means "use this dir" in bd's resolution —
			// but only a dir with real store content qualifies as a final
			// target; an empty pointer over an empty dir is a broken store.
			if hasStoreMarkers(beadsDir) {
				return beadsDir
			}
			return ""
		}
		if !filepath.IsAbs(target) {
			target = filepath.Clean(filepath.Join(filepath.Dir(beadsDir), target))
		}
		beadsDir = target
	}
	return ""
}

// isCanonicalStore reports whether beadsDir looks like a usable store or
// redirect pointer (rather than a stray bootstrap).
func isCanonicalStore(beadsDir string) bool {
	for _, marker := range []string{"redirect", "config.yaml", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(beadsDir, marker)); err == nil {
			return true
		}
	}
	return false
}

// hasStoreMarkers reports whether beadsDir carries real store content (not
// counting a redirect pointer).
func hasStoreMarkers(beadsDir string) bool {
	for _, marker := range []string{"config.yaml", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(beadsDir, marker)); err == nil {
			return true
		}
	}
	return false
}
