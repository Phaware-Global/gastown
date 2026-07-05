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

// ErrCodegraphDisabled indicates codegraph indexing is turned off for the
// town/rig, so no index was built. Distinct from ErrCodegraphUnavailable so a
// caller can report "disabled" rather than a false "index refreshed".
var ErrCodegraphDisabled = errors.New("codegraph indexing disabled")

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
// silently degrading to Read/Grep. Returns ErrCodegraphDisabled when indexing is
// off for the town/rig and ErrCodegraphUnavailable when the executable can't be
// located — distinct signals so the caller reports "disabled" vs "missing"
// accurately instead of a misleading success.
func EnsureCodegraphIndexSync(worktreePath, townRoot, rigPath string) error {
	enabled, err := codegraphEnabled(townRoot, rigPath)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrCodegraphDisabled
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

	var cmd *exec.Cmd
	// The PATH prepend must point at the bin dir of the Node actually used to run
	// codegraph, so a child `#!/usr/bin/env node` resolves to that same Node and
	// not the worktree's pinned one. Default to codegraph's own bin (standalone
	// binary / direct run); override to the chosen Node's bin below.
	pathDir := filepath.Dir(exe)
	// A `#!/usr/bin/env node` codegraph shim (mise or npm-global) must be run with
	// an explicit Node, or the worktree's pinned Node hijacks the env-node shebang
	// — the exact failure this resolver routes around. A standalone compiled binary
	// (no node shebang) is run directly.
	if node := nodeForCodegraph(exe); node != "" {
		cmd = exec.CommandContext(ctx, node, exe, subcmd, worktreePath)
		pathDir = filepath.Dir(node)
	} else {
		cmd = exec.CommandContext(ctx, exe, subcmd, worktreePath)
	}
	cmd.Dir = worktreePath
	cmd.Env = codegraphEnv(pathDir)
	return cmd, true
}

// nodeForCodegraph returns a Node interpreter to run a `#!/usr/bin/env node`
// codegraph shim with, chosen independently of the worktree's pinned Node.
// Returns "" for a standalone compiled binary (no node shebang) or when no Node
// can be found, in which case codegraph is run directly. Prefers the sibling
// node in codegraph's own bin (the mise-install case); for an npm-global shim
// whose bin has no node, falls back to the highest mise Node install so a
// worktree Node pin still can't hijack the run.
func nodeForCodegraph(exe string) string {
	if !isNodeShebang(exe) {
		return ""
	}
	if sib := filepath.Join(filepath.Dir(exe), "node"); fileExists(sib) {
		return sib
	}
	return highestMiseNode()
}

// isNodeShebang reports whether exe begins with a `#!…node` shebang line (i.e. a
// JS entrypoint that needs a Node interpreter), as opposed to a compiled binary.
func isNodeShebang(exe string) bool {
	f, err := os.Open(exe) //nolint:gosec // G304: exe is a resolved codegraph path, not user input
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 128)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.HasPrefix(line, "#!") && strings.Contains(line, "node")
}

// codegraphEnv returns the environment for a codegraph run: the process
// environment with credential-bearing variables stripped, then nodeBinDir (the
// bin dir of the Node used to run codegraph) prepended to PATH — so any
// `node`/child resolution shadows the mise shim and the worktree's pinned Node.
// The reviewer runs the index over an external contributor's checked-out PR
// head, so secrets like GITHUB_TOKEN must not be in scope — codegraph is a
// static indexer, but not handing it the reviewer's credentials is cheap
// defense-in-depth if it ever touches repo-resolved tooling.
func codegraphEnv(nodeBinDir string) []string {
	origPath := os.Getenv("PATH")
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		name := kv[:i]
		// PATH is rebuilt below; drop the original to avoid a duplicate key.
		if name == "PATH" || isSensitiveEnv(name) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "PATH="+nodeBinDir+string(os.PathListSeparator)+origPath)
}

// isSensitiveEnv reports whether an environment variable name looks like it
// carries a secret (token, key, password, credential).
func isSensitiveEnv(name string) bool {
	up := strings.ToUpper(name)
	for _, needle := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "_KEY", "APIKEY", "ACCESS_KEY"} {
		if strings.Contains(up, needle) {
			return true
		}
	}
	return false
}

// resolveCodegraphExe finds the codegraph executable, robust to a Node-version
// pin that would send a PATH lookup to the mise shim. It accepts any PATH hit
// that is not the mise shim — a real mise install bin OR a standalone/global
// install (npm-global, Homebrew, the standalone installer) — and only falls back
// to scanning the mise Node installs when PATH yields nothing usable.
func resolveCodegraphExe() string {
	if p, err := exec.LookPath("codegraph"); err == nil && !isMiseShim(p) {
		return p
	}
	return bestMiseCodegraph(miseCodegraphCandidates())
}

// isMiseShim reports whether p is a mise shim (…/mise/shims/…). A shim delegates
// to the mise binary, which refuses to run codegraph when the cwd pins a Node
// version other than codegraph's — the exact failure this resolver routes around.
func isMiseShim(p string) bool {
	return strings.Contains(filepath.ToSlash(p), "/shims/")
}

// bestMiseCodegraph picks the codegraph under the highest concrete Node version.
func bestMiseCodegraph(candidates []string) string {
	return bestByNodeVersion(candidates)
}

// highestMiseNode returns the `node` from the highest concrete mise Node install,
// or "" if none. Used to run an npm-global codegraph shim with a deterministic
// Node when it has no sibling node of its own.
func highestMiseNode() string {
	dataDir := miseDataDir()
	if dataDir == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "installs", "node", "*", "bin", "node"))
	return bestByNodeVersion(matches)
}

// bestByNodeVersion picks the path under …/installs/node/<ver>/bin/… with the
// highest concrete Node version, preferring numeric version dirs over mise
// aliases (lts/latest/system) so the choice is deterministic regardless of glob
// order (a lexical sort would rank an alias like "lts" above "24.14.1").
func bestByNodeVersion(paths []string) string {
	best := ""
	var bestVer []int
	bestConcrete := false
	for _, p := range paths {
		if !fileExists(p) {
			continue
		}
		// …/installs/node/<ver>/bin/<name>
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

// miseCodegraphCandidates returns every codegraph executable under the mise Node
// installs.
func miseCodegraphCandidates() []string {
	dataDir := miseDataDir()
	if dataDir == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "installs", "node", "*", "bin", "codegraph"))
	return matches
}

// miseDataDir returns the mise data directory, honoring MISE_DATA_DIR and
// falling back to the default (~/.local/share/mise).
func miseDataDir() string {
	if d := os.Getenv("MISE_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "mise")
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
