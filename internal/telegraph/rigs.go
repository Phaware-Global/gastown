package telegraph

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// LoadRigGitHubRepos reads the town's rig registry at <townRoot>/mayor/rigs.json
// and returns the set of GitHub "owner/repo" identifiers parsed from each
// rig's git_url. Rigs whose URL is empty or doesn't point at github.com
// contribute nothing; this is the documented behavior — operators wanting
// Telegraph coverage for a non-rig GitHub repo (e.g. an infrastructure repo)
// use the per-provider ExtraRepos knob.
//
// A missing rigs.json is not an error and yields an empty slice — Telegraph
// is usable without any rigs registered (an operator might run Telegraph for
// extra_repos only).
//
// Results are de-duplicated and order-preserving so the operator-facing
// log line in cmd/telegraph.go is deterministic across restarts.
func LoadRigGitHubRepos(townRoot string) ([]string, error) {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	cfg, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading rigs registry %s: %w", rigsPath, err)
	}
	if cfg == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(cfg.Rigs))
	out := make([]string, 0, len(cfg.Rigs))
	for _, entry := range cfg.Rigs {
		repo, ok := parseGitHubRepoURL(entry.GitURL)
		if !ok {
			continue
		}
		key := strings.ToLower(repo)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, repo)
	}
	return out, nil
}

// parseGitHubRepoURL extracts "owner/repo" from a git remote URL. Supported
// shapes (the two GitHub actually issues):
//
//   - https://github.com/owner/repo
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo
//   - git@github.com:owner/repo.git
//   - ssh://git@github.com/owner/repo(.git)
//
// Any host other than github.com (or its trailing-dot canonical form) is
// rejected so a rig pointed at GitLab / Bitbucket doesn't accidentally
// allow-list a similarly-named GitHub repo. Returns the parsed identifier
// (preserving the original case so the audit log matches the rig registry)
// and ok=true on a clean parse.
func parseGitHubRepoURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}

	var rest string
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		rest = strings.TrimPrefix(raw, "git@github.com:")
	case strings.HasPrefix(raw, "ssh://git@github.com/"):
		rest = strings.TrimPrefix(raw, "ssh://git@github.com/")
	case strings.HasPrefix(raw, "https://github.com/"):
		rest = strings.TrimPrefix(raw, "https://github.com/")
	case strings.HasPrefix(raw, "http://github.com/"):
		rest = strings.TrimPrefix(raw, "http://github.com/")
	default:
		return "", false
	}

	// Strip optional ".git" suffix and any trailing slash / fragment.
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	if idx := strings.IndexAny(rest, "#?"); idx >= 0 {
		rest = rest[:idx]
	}

	// Must be exactly "owner/repo" — reject deeper paths (subdirs, blob URLs,
	// etc.) since those don't identify a repository at the webhook layer.
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}
