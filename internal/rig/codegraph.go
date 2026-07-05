package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/util"
)

// codegraphSyncTimeout bounds a synchronous index build so a caller (e.g. the
// reviewer checkout) can't hang indefinitely on a pathological repo.
const codegraphSyncTimeout = 10 * time.Minute

// ErrCodegraphUnavailable indicates codegraph indexing is enabled but the
// codegraph executable could not be located. Callers that need a deterministic
// index (the reviewer) surface this loudly rather than reviewing with no index.
var ErrCodegraphUnavailable = errors.New("codegraph executable not found")

// EnsureCodegraphIndex launches a background codegraph index build on the
// worktree when indexing is enabled for this town/rig. Non-fatal and
// best-effort: used for polecat/refinery worktrees where the index can warm up
// while the agent works. Returns nil when indexing is disabled or the executable
// is absent; an error only on a real config-load failure or a failed process
// start, so callers can surface it with their own context.
func EnsureCodegraphIndex(worktreePath, townRoot, rigPath string) error {
	enabled, err := codegraphEnabled(townRoot, rigPath)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}

	cmd, ok := codegraphCommand(context.Background(), worktreePath, codegraphSubcommand(worktreePath))
	if !ok {
		return nil // executable absent — silently skip on the background path
	}
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting codegraph indexing: %w", err)
	}
	// Reap the detached process to prevent zombie/defunct entries in a long-running daemon.
	go func() { _ = cmd.Wait() }()
	return nil
}

// EnsureCodegraphIndexSync builds the codegraph index synchronously and waits
// for completion, guaranteeing a fresh index before the caller proceeds. The
// reviewer checkout uses this so a review runs against a real index instead of
// silently degrading to Read/Grep. Returns ErrCodegraphUnavailable when the
// executable can't be located so the caller can warn instead of reviewing blind;
// nil when indexing is disabled.
func EnsureCodegraphIndexSync(worktreePath, townRoot, rigPath string) error {
	enabled, err := codegraphEnabled(townRoot, rigPath)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), codegraphSyncTimeout)
	defer cancel()

	cmd, ok := codegraphCommand(ctx, worktreePath, codegraphSubcommand(worktreePath))
	if !ok {
		return ErrCodegraphUnavailable
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codegraph indexing failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// codegraphSubcommand picks `init` for a fresh worktree or `index` (a full
// reindex) when .codegraph already exists. A reviewer worktree is reused across
// PR checkouts, so a prior index must be rebuilt to match the newly checked-out
// HEAD rather than left stale.
func codegraphSubcommand(worktreePath string) string {
	if _, err := os.Stat(filepath.Join(worktreePath, ".codegraph")); err == nil {
		return "index"
	}
	return "init"
}

// codegraphEnabled reports whether codegraph indexing is enabled for this
// town/rig (town default true, rig override wins).
func codegraphEnabled(townRoot, rigPath string) (bool, error) {
	townSettings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return false, fmt.Errorf("loading town settings: %w", err)
	}
	rigSettings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return false, fmt.Errorf("loading rig settings: %w", err)
	}
	return config.IsCodeGraphIndexingEnabled(townSettings, rigSettings), nil
}

// codegraphCommand builds a command that runs codegraph deterministically,
// immune to the worktree's Node version pin. codegraph is installed under one
// specific mise-managed Node; inside a repo that pins a different Node (a
// .node-version / .nvmrc / .tool-versions / mise.toml file), the `codegraph`
// shim resolves to mise, which refuses ("codegraph is a mise bin however it is
// not currently active"), and the shim's `#!/usr/bin/env node` shebang would
// otherwise select the pinned Node. We therefore locate codegraph inside a mise
// Node install and run it with that install's own `node`, on an install-bin-first
// PATH so any child `node` resolution stays on the correct Node too. Returns
// false when no codegraph executable can be found.
func codegraphCommand(ctx context.Context, worktreePath, subcmd string) (*exec.Cmd, bool) {
	exe := resolveCodegraphExe()
	if exe == "" {
		return nil, false
	}

	binDir := filepath.Dir(exe)
	var cmd *exec.Cmd
	if node := filepath.Join(binDir, "node"); fileExists(node) {
		// Run the script under its sibling node explicitly, bypassing the
		// env-node shebang entirely.
		cmd = exec.CommandContext(ctx, node, exe, subcmd, worktreePath)
	} else {
		cmd = exec.CommandContext(ctx, exe, subcmd, worktreePath)
	}
	cmd.Dir = worktreePath
	// Prepend codegraph's own Node bin so any `node`/child resolution shadows the
	// mise shim and the worktree's pinned Node.
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return cmd, true
}

// resolveCodegraphExe finds the codegraph executable in a mise Node install,
// robust to a Node-version pin that would send a PATH lookup to the mise shim.
// Prefers a PATH hit that already points at a real install bin (the active
// global Node's codegraph), then scans the mise Node installs directly.
func resolveCodegraphExe() string {
	if p, err := exec.LookPath("codegraph"); err == nil && isMiseNodeInstallBin(p) {
		return p
	}
	return bestMiseCodegraph(miseCodegraphCandidates())
}

// bestMiseCodegraph picks the codegraph under the highest concrete Node version,
// preferring numeric version dirs over mise aliases (lts/latest/system). This
// keeps the choice deterministic regardless of glob order, where a lexical sort
// would rank an alias like "lts" above "24.14.1".
func bestMiseCodegraph(candidates []string) string {
	best := ""
	var bestVer []int
	bestConcrete := false
	for _, p := range candidates {
		if !fileExists(p) {
			continue
		}
		// …/installs/node/<ver>/bin/codegraph
		ver := filepath.Base(filepath.Dir(filepath.Dir(p)))
		parts, concrete := parseNodeVersion(ver)
		switch {
		case best == "":
			best, bestVer, bestConcrete = p, parts, concrete
		case concrete && !bestConcrete:
			best, bestVer, bestConcrete = p, parts, concrete
		case concrete == bestConcrete && compareNodeVersions(parts, bestVer) > 0:
			best, bestVer = p, parts
		}
	}
	return best
}

// parseNodeVersion splits a dotted numeric version into ints. concrete is false
// for mise aliases (lts, latest, system, …) that don't parse as a version.
func parseNodeVersion(v string) (parts []int, concrete bool) {
	for _, seg := range strings.Split(v, ".") {
		n, err := strconv.Atoi(seg)
		if err != nil {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, len(parts) > 0
}

// compareNodeVersions returns >0 if a is newer than b, <0 if older, 0 if equal.
func compareNodeVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var ai, bi int
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

// isMiseNodeInstallBin reports whether p is a real mise Node install bin
// (…/mise/installs/node/<ver>/bin/codegraph), not the …/mise/shims/codegraph
// shim (which points at the mise binary and is Node-pin sensitive).
func isMiseNodeInstallBin(p string) bool {
	s := filepath.ToSlash(p)
	return strings.Contains(s, "/installs/node/") && !strings.Contains(s, "/shims/")
}

// miseCodegraphCandidates returns every codegraph executable under the mise Node
// installs, honoring MISE_DATA_DIR and falling back to the default data dir.
func miseCodegraphCandidates() []string {
	dataDir := os.Getenv("MISE_DATA_DIR")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dataDir = filepath.Join(home, ".local", "share", "mise")
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "installs", "node", "*", "bin", "codegraph"))
	return matches
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
