package reviewer

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	if rigPath != "" {
		p := perspectiveRelPath(rigPath, name)
		if data, err := os.ReadFile(p); err == nil {
			return ResolvedPerspective{Name: name, Source: PerspectiveSourceRig, Path: p, Content: string(data)}, nil
		}
	}
	if townRoot != "" {
		p := perspectiveRelPath(townRoot, name)
		if data, err := os.ReadFile(p); err == nil {
			return ResolvedPerspective{Name: name, Source: PerspectiveSourceTown, Path: p, Content: string(data)}, nil
		}
	}
	if data, err := defaultPerspectivesFS.ReadFile("perspectives/" + name + ".md"); err == nil {
		return ResolvedPerspective{
			Name:    name,
			Source:  PerspectiveSourceBuiltin,
			Path:    "embedded:" + name,
			Content: string(data),
		}, nil
	}
	return ResolvedPerspective{}, fmt.Errorf("perspective %q not found (looked in rig, town, and built-in defaults)", name)
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
			if failSilent {
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
