package reviewer

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPerspectiveNotFound is returned (wrapped) by ResolvePerspective when a
// perspective name has no file at any tier and no embedded default. It is
// distinct from validation errors (path traversal) and real I/O errors, so
// fail-silent callers can skip only genuinely-missing perspectives while still
// surfacing misconfiguration and disk/permission failures.
var ErrPerspectiveNotFound = errors.New("perspective not found")

//go:embed perspectives/*.md
var defaultPerspectivesFS embed.FS

// PerspectiveSource identifies where a resolved perspective prompt came from.
type PerspectiveSource string

const (
	// PerspectiveSourceRig is a rig-local perspective file.
	PerspectiveSourceRig PerspectiveSource = "rig"
	// PerspectiveSourceTown is a town-shared perspective file.
	PerspectiveSourceTown PerspectiveSource = "town"
	// PerspectiveSourceBuiltin is an embedded default perspective.
	PerspectiveSourceBuiltin PerspectiveSource = "builtin"
)

// ResolvedPerspective is a perspective prompt plus where it was found.
type ResolvedPerspective struct {
	Name    string
	Source  PerspectiveSource
	Path    string // filesystem path for rig/town; "embedded:<name>" for builtin
	Content string
}

// perspectiveRelPath returns the conventional settings path for a perspective
// file under a rig or town root: settings/review/perspectives/<name>.md.
func perspectiveRelPath(root, name string) string {
	return filepath.Join(root, "settings", "review", "perspectives", name+".md")
}

// ResolvePerspective resolves a single perspective prompt by name, in order:
//
//	<rigPath>/settings/review/perspectives/<name>.md      (rig override)
//	<townRoot>/settings/review/perspectives/<name>.md     (town-shared)
//	embedded default (adversarial, security)              (builtin)
//
// rigPath may be empty (town/standalone resolution). A name with no file at any
// tier and no embedded default returns an error so a misconfigured
// `review.perspectives` entry surfaces loudly rather than silently vanishing.
func ResolvePerspective(townRoot, rigPath, name string) (ResolvedPerspective, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ResolvedPerspective{}, fmt.Errorf("perspective name must not be empty")
	}
	// Reject path-traversal / separator characters before the name is ever
	// joined into a filesystem path. Perspective names come from rig config
	// (and, in later phases, possibly attacker-influenced sources), so a name
	// like "../../etc/passwd" must not escape the perspectives directory.
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return ResolvedPerspective{}, fmt.Errorf(
			"invalid perspective name %q: must not contain path separators or %q", name, "..")
	}
	// readTier reads a perspective file from a rig/town root, distinguishing
	// "file absent here, try the next tier" (nil data, nil err) from a real I/O
	// error (permission denied, disk error) which must surface rather than be
	// silently swallowed as a fallback.
	readTier := func(root string, src PerspectiveSource) (*ResolvedPerspective, error) {
		if root == "" {
			return nil, nil
		}
		p := perspectiveRelPath(root, name)
		data, err := os.ReadFile(p)
		if err == nil {
			return &ResolvedPerspective{Name: name, Source: src, Path: p, Content: string(data)}, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading %s perspective %s: %w", src, p, err)
		}
		return nil, nil
	}
	if rp, err := readTier(rigPath, PerspectiveSourceRig); err != nil {
		return ResolvedPerspective{}, err
	} else if rp != nil {
		return *rp, nil
	}
	if rp, err := readTier(townRoot, PerspectiveSourceTown); err != nil {
		return ResolvedPerspective{}, err
	} else if rp != nil {
		return *rp, nil
	}
	if data, err := defaultPerspectivesFS.ReadFile("perspectives/" + name + ".md"); err == nil {
		return ResolvedPerspective{
			Name:    name,
			Source:  PerspectiveSourceBuiltin,
			Path:    "embedded:" + name,
			Content: string(data),
		}, nil
	}
	return ResolvedPerspective{}, fmt.Errorf("perspective %q not found (looked in rig, town, and built-in defaults): %w", name, ErrPerspectiveNotFound)
}

// ResolvePerspectives resolves a list of perspective names in order. When
// failSilent is false (the default), the first unresolved name is a hard error;
// when true, unresolved names are skipped and returned in the second result so
// the caller can log what was dropped.
func ResolvePerspectives(townRoot, rigPath string, names []string, failSilent bool) ([]ResolvedPerspective, []string, error) {
	var resolved []ResolvedPerspective
	var skipped []string
	for _, name := range names {
		rp, err := ResolvePerspective(townRoot, rigPath, name)
		if err != nil {
			// Even in fail-silent mode, only a genuinely-missing perspective is
			// skippable. A path-traversal name or a real I/O error is
			// misconfiguration/operational failure and must surface, never be
			// silently dropped.
			if failSilent && errors.Is(err, ErrPerspectiveNotFound) {
				skipped = append(skipped, name)
				continue
			}
			return nil, nil, err
		}
		resolved = append(resolved, rp)
	}
	return resolved, skipped, nil
}

// BuiltinPerspectiveNames returns the names of the embedded default
// perspectives, sorted, for help text and tests.
func BuiltinPerspectiveNames() []string {
	entries, err := defaultPerspectivesFS.ReadDir("perspectives")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		n := strings.TrimSuffix(e.Name(), ".md")
		if n != e.Name() {
			names = append(names, n)
		}
	}
	return names
}
