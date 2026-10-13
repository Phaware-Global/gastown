package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// maxBeadsScanDepth bounds the walk for .beads stores under the town root. The
// deepest real store is a rig's mayor/rig/.beads (3 below the town root); crew
// and polecat clones sit one level deeper.
const maxBeadsScanDepth = 5

// AutoStartConfigCheck verifies every .beads store has dolt.auto-start: false.
// With the shared Dolt server unreachable (reboot, restart), bd launches its own
// sql-server from the store's .beads/dolt dir on the town's port, and the first
// store to bind wins — agents then read an empty database. The session env var
// BEADS_DOLT_AUTO_START=0 does not cover every bd caller, so the config is the
// durable guard.
type AutoStartConfigCheck struct {
	FixableCheck
	issues []autoStartIssue // found by Run, consumed by CanFix and Fix
}

// autoStartIssue is one .beads store that can auto-start, and whether Fix can
// repair it. A store that cannot be repaired (a refused config.yaml, or no
// prefix to write a new one with) is still reported; it is never passed over.
type autoStartIssue struct {
	dir     string
	missing bool   // no config.yaml; Fix creates the full default config
	reason  string // non-empty: not fixable, and why
}

// NewAutoStartConfigCheck creates a new auto-start config check.
func NewAutoStartConfigCheck() *AutoStartConfigCheck {
	return &AutoStartConfigCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "auto-start-config",
				CheckDescription: "Verify every .beads store disables bd's Dolt auto-start (dolt.auto-start: false)",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// CanFix reports whether the last Run found anything Fix can repair.
func (c *AutoStartConfigCheck) CanFix() bool {
	for _, is := range c.issues {
		if is.reason == "" {
			return true
		}
	}
	return false
}

// Run scans the town for .beads stores that can auto-start: no config.yaml, or
// one that does not set dolt.auto-start: false.
func (c *AutoStartConfigCheck) Run(ctx *CheckContext) *CheckResult {
	c.issues = nil
	var checked int
	for _, dir := range findBeadsStores(ctx.TownRoot) {
		checked++
		data, err := readRegularConfig(filepath.Join(dir, "config.yaml"))
		switch {
		case errors.Is(err, os.ErrNotExist):
			// bd falls back to its defaults, which auto-start. Fix writes the
			// full default config (prefix included) rather than a one-liner,
			// which would make later repairs see a file and skip.
			is := autoStartIssue{dir: dir, missing: true}
			if beads.ConfigDefaultsFromMetadata(dir, "") == "" {
				is.reason = "config.yaml is missing and metadata.json has no prefix to create it with"
			}
			c.issues = append(c.issues, is)
		case err != nil:
			c.issues = append(c.issues, autoStartIssue{dir: dir, reason: "refused: " + err.Error()})
		case !configDisablesAutoStart(string(data)):
			c.issues = append(c.issues, autoStartIssue{dir: dir})
		}
	}

	if len(c.issues) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d .beads stores have dolt.auto-start: false", checked),
		}
	}

	details := make([]string, len(c.issues))
	unfixable := 0
	for i, is := range c.issues {
		details[i] = relToTown(ctx.TownRoot, is.dir)
		switch {
		case is.reason != "":
			details[i] += " (cannot fix: " + is.reason + ")"
			unfixable++
		case is.missing:
			details[i] += " (config.yaml missing)"
		}
	}
	hint := "Run 'gt doctor --fix' to set dolt.auto-start: false"
	if unfixable > 0 {
		hint = fmt.Sprintf("%d store(s) need manual repair: set dolt.auto-start: false in their .beads/config.yaml", unfixable)
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d .beads store(s) can auto-start a Dolt server on the shared port", len(c.issues)),
		Details: details,
		FixHint: hint,
	}
}

// Fix repairs every fixable store Run flagged and keeps going past failures. It
// returns an error naming each store it could not repair or refused to touch.
func (c *AutoStartConfigCheck) Fix(ctx *CheckContext) error {
	var errs []error
	for _, is := range c.issues {
		rel := relToTown(ctx.TownRoot, is.dir)
		switch {
		case is.reason != "":
			errs = append(errs, fmt.Errorf("%s: %s", rel, is.reason))
		case is.missing:
			if err := beads.EnsureConfigYAMLFromMetadataIfMissing(is.dir, ""); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			}
		default:
			if err := setConfigAutoStartDisabled(is.dir); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			}
		}
	}
	return errors.Join(errs...)
}

// readRegularConfig reads path only if it is a regular file. A symlinked
// config.yaml (or a directory/device in its place) is refused rather than
// followed, so a repair never reads or rewrites a file outside the store.
func readRegularConfig(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode().Type())
	}
	return os.ReadFile(path)
}

// setConfigAutoStartDisabled rewrites dir/config.yaml with dolt.auto-start set
// to false, leaving every other line untouched. It never creates the file (see
// Run) and refuses a non-regular file. The write goes to a temp file renamed
// over the original, so a symlink swapped in after the check is replaced, not
// followed.
func setConfigAutoStartDisabled(beadsDir string) error {
	path := filepath.Join(beadsDir, "config.yaml")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	data, err := readRegularConfig(path)
	if err != nil {
		return err
	}
	const want = "dolt.auto-start: false"
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "dolt.auto-start:") {
			lines[i] = want
			found = true
		}
	}
	out := strings.Join(lines, "\n")
	if !found {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += want
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	tmp, err := os.CreateTemp(beadsDir, ".config.yaml.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// configDisablesAutoStart reports whether config.yaml content sets
// dolt.auto-start to false. Comments do not count.
func configDisablesAutoStart(content string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "dolt.auto-start:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "dolt.auto-start:"))
			return strings.EqualFold(strings.Trim(value, `"'`), "false")
		}
	}
	return false
}

// findBeadsStores returns every .beads dir under townRoot that holds its own
// store (has a metadata.json). Redirect-only .beads dirs, which point at
// another store, are skipped.
func findBeadsStores(townRoot string) []string {
	var stores []string
	_ = filepath.WalkDir(townRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(townRoot, path)
		if relErr != nil {
			return filepath.SkipDir
		}
		if rel != "." && strings.Count(rel, string(filepath.Separator))+1 > maxBeadsScanDepth {
			return filepath.SkipDir
		}
		switch d.Name() {
		case ".git", ".dolt", ".dolt-data", "node_modules":
			return filepath.SkipDir
		case ".beads":
			if _, statErr := os.Stat(filepath.Join(path, "metadata.json")); statErr == nil {
				stores = append(stores, path)
			}
			return filepath.SkipDir
		}
		return nil
	})
	return stores
}

func relToTown(townRoot, path string) string {
	if rel, err := filepath.Rel(townRoot, path); err == nil {
		return rel
	}
	return path
}
