package plugin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// fetchTimeout and archiveTimeout bound the git subprocesses SyncFromOrigin
// runs. Without a deadline, a stalled network fetch blocks whichever
// goroutine calls SyncFromOrigin indefinitely — in the daemon's case, that
// goroutine also needs to notice shutdown promptly (PR #236 review).
const (
	fetchTimeout   = 2 * time.Minute
	archiveTimeout = 1 * time.Minute
)

// SyncResult records the outcome of a plugin sync operation.
type SyncResult struct {
	Copied  []string // plugin names that were copied/updated
	Removed []string // plugin names that were removed (clean mode)
	Skipped []string // plugin names that were already up-to-date
	Errors  []string // errors encountered
}

// SyncPlugins copies plugin directories from source to target.
// If clean is true, removes plugins from target that don't exist in source.
func SyncPlugins(sourceDir, targetDir string, clean bool) (*SyncResult, error) {
	result := &SyncResult{}

	srcInfo, err := os.Stat(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("source directory %s: %w", sourceDir, err)
	}
	if !srcInfo.IsDir() {
		return nil, fmt.Errorf("source is not a directory: %s", sourceDir)
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("creating target directory: %w", err)
	}

	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("reading source directory: %w", err)
	}

	srcPlugins := make(map[string]bool)
	for _, entry := range srcEntries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		pluginMD := filepath.Join(sourceDir, entry.Name(), "plugin.md")
		if _, err := os.Stat(pluginMD); err != nil {
			continue // Not a plugin directory
		}
		srcPlugins[entry.Name()] = true

		srcPluginDir := filepath.Join(sourceDir, entry.Name())
		dstPluginDir := filepath.Join(targetDir, entry.Name())

		if dirsMatch(srcPluginDir, dstPluginDir) {
			result.Skipped = append(result.Skipped, entry.Name())
			continue
		}

		if err := copyDir(srcPluginDir, dstPluginDir); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		result.Copied = append(result.Copied, entry.Name())
	}

	if clean {
		dstEntries, err := os.ReadDir(targetDir)
		if err == nil {
			for _, entry := range dstEntries {
				if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
					continue
				}
				if !srcPlugins[entry.Name()] {
					dstPath := filepath.Join(targetDir, entry.Name())
					if err := os.RemoveAll(dstPath); err != nil {
						result.Errors = append(result.Errors, fmt.Sprintf("removing %s: %v", entry.Name(), err))
					} else {
						result.Removed = append(result.Removed, entry.Name())
					}
				}
			}
		}
	}

	return result, nil
}

// dirsMatch checks if two plugin directories have identical contents.
func dirsMatch(src, dst string) bool {
	srcHash := DirHash(src)
	dstHash := DirHash(dst)
	return srcHash != "" && srcHash == dstHash
}

// DirHash computes a content hash of all files in a directory.
func DirHash(dir string) string {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: walking trusted plugin directory
		if err != nil {
			return err
		}
		h.Write(data)
		return nil
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// copyDir recursively copies a directory, replacing the destination atomically.
// It copies to a temp directory in the same parent, then swaps via rename.
func copyDir(src, dst string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(dst), ".plugin-sync-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	// Clean up temp dir on failure; on success it's been renamed away.
	defer os.RemoveAll(tmpDir)

	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		tmpPath := filepath.Join(tmpDir, rel)
		if d.IsDir() {
			return os.MkdirAll(tmpPath, 0755)
		}
		return copyFile(path, tmpPath)
	}); err != nil {
		return err
	}

	// Atomic swap: remove old dst, rename temp into place.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing old destination: %w", err)
	}
	return os.Rename(tmpDir, dst)
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode()) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// FindGastownSource locates the gastown source repo's plugins directory.
// Search order:
//  1. Walk up from CWD for a gastown go.mod with plugins/
//  2. <townRoot>/gastown/crew/den/plugins/
//  3. <townRoot>/gastown/plugins/
func FindGastownSource(townRoot string) (string, error) {
	if cwd, err := os.Getwd(); err == nil {
		if src := findSourceFromDir(cwd); src != "" {
			return src, nil
		}
	}

	candidates := []string{
		filepath.Join(townRoot, "gastown", "crew", "den", "plugins"),
		filepath.Join(townRoot, "gastown", "plugins"),
	}
	for _, candidate := range candidates {
		if hasPlugins(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not locate gastown plugin source; use --source to specify")
}

func findSourceFromDir(dir string) string {
	current := dir
	for {
		pluginsDir := filepath.Join(current, "plugins")
		goMod := filepath.Join(current, "go.mod")
		if hasPlugins(pluginsDir) {
			if isGastownModule(goMod) {
				return pluginsDir
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

// isGastownModule checks if a go.mod file declares a gastown module path.
// Matches "module .../gastown" on the module directive line to avoid
// false-positives from comments or dependency names.
func isGastownModule(goModPath string) bool {
	f, err := os.Open(goModPath) //nolint:gosec // G304: path from traversal
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.HasSuffix(line, "/gastown") || line == "module gastown"
		}
	}
	return false
}

// FindGastownGitDir locates a persistent git checkout of the gastown repo
// suitable for fetching and archiving origin/main — e.g. <townRoot>/gastown/mayor/rig,
// the Mayor's long-lived working clone (unlike a polecat worktree, which is
// nuked on `gt done`).
//
// Unlike FindGastownSource, this does NOT require the checkout's working tree
// to already contain an up-to-date plugins/ directory: SyncFromOrigin reads
// the plugins/ tree straight out of the checkout's git object store at
// origin/main, so the checkout's own branch and staleness are irrelevant.
// This is what lets plugin sync stay correct even when every on-disk
// checkout has drifted from main (gt-2ea1: the deployed dolt-archive plugin
// was 3 months stale because every prior mechanism depended on some working
// tree being kept current, and none was).
func FindGastownGitDir(townRoot string) (string, error) {
	if cwd, err := os.Getwd(); err == nil {
		if dir := findGitDirFromDir(cwd); dir != "" {
			return dir, nil
		}
	}

	candidates := []string{
		filepath.Join(townRoot, "gastown", "mayor", "rig"),
		filepath.Join(townRoot, "gastown", "refinery", "rig"),
		filepath.Join(townRoot, "gastown", "reviewer", "rig"),
		filepath.Join(townRoot, "gastown", "crew", "den"),
		filepath.Join(townRoot, "gastown"),
	}
	for _, candidate := range candidates {
		if isGastownModule(filepath.Join(candidate, "go.mod")) {
			if _, err := os.Stat(filepath.Join(candidate, ".git")); err == nil {
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("could not locate a gastown git checkout; use --source to specify")
}

func findGitDirFromDir(dir string) string {
	current := dir
	for {
		if isGastownModule(filepath.Join(current, "go.mod")) {
			if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

// SyncFromOrigin fetches origin/main into gitDir and syncs the named
// plugins/<name> directories into targetDir. It reads the tree directly from
// git's object store via `git archive`, so it never touches gitDir's working
// tree or index — the checkout can be sitting on an unrelated or stale branch
// and this still deploys the true origin/main content.
//
// pluginNames must be explicit and non-empty — this does NOT sync "every
// plugin in the repo". A townRoot/plugins checkout can accumulate plugins
// whose deployed copy has diverged from the repo in the OTHER direction: a
// hand-applied production fix that was never committed back (confirmed for
// at least two plugins during the gt-2ea1 investigation — see gt-bpew).
// Blanket-syncing from repo->target would silently overwrite those with
// older, unfixed code. Callers must name only the plugins they've verified
// are safe to make repo-authoritative.
func SyncFromOrigin(ctx context.Context, gitDir, targetDir string, pluginNames []string, clean bool) (*SyncResult, error) {
	if len(pluginNames) == 0 {
		return nil, fmt.Errorf("no plugin names specified — refusing to sync all plugins blindly")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	// Fetch only refs/heads/main, into the fully-qualified tracking ref, via
	// an explicit refspec — never the ambiguous short name "origin/main".
	// git's short-name DWIM resolution checks refs/tags/<name> before
	// refs/remotes/<name>, so anyone who can push a tag named "origin/main"
	// could otherwise have it resolved and deployed to every plugin
	// directory in town, bypassing main's branch protection entirely
	// (PR #236 review).
	if err := runGit(fetchCtx, gitDir, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return nil, fmt.Errorf("fetching origin/main: %w", err)
	}

	revCtx, revCancel := context.WithTimeout(ctx, archiveTimeout)
	defer revCancel()
	sha, err := revParse(revCtx, gitDir, "refs/remotes/origin/main")
	if err != nil {
		return nil, fmt.Errorf("resolving origin/main: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "gastown-plugin-sync-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	archiveCtx, archiveCancel := context.WithTimeout(ctx, archiveTimeout)
	defer archiveCancel()
	// Archive the resolved SHA, not a ref name: once resolved, there is no
	// remaining name for a tag to shadow.
	if err := archivePluginsTree(archiveCtx, gitDir, sha, tmpDir, pluginNames); err != nil {
		return nil, fmt.Errorf("extracting plugins/ from origin/main (%s): %w", sha, err)
	}

	sourceDir := filepath.Join(tmpDir, "plugins")
	if _, err := os.Stat(sourceDir); err != nil {
		return nil, fmt.Errorf("origin/main has none of the requested plugins/ directories")
	}

	return SyncPlugins(sourceDir, targetDir, clean)
}

// gitSubprocessEnv returns the environment for a git child process, with
// terminal credential prompts disabled — a daemon has no terminal to prompt,
// so without this a git call against a remote requiring auth would hang
// instead of failing fast.
func gitSubprocessEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
}

// gitWaitDelay bounds how long Wait() blocks on I/O cleanup after a git/tar
// subprocess exits or is killed via context cancellation — without it, a
// grandchild process holding the stdout/stderr pipe open could hang Wait()
// past the context deadline that was supposed to bound the whole call.
const gitWaitDelay = 10 * time.Second

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed subcommand, dir is caller-controlled
	cmd.Env = gitSubprocessEnv()
	cmd.WaitDelay = gitWaitDelay
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// revParse resolves ref to a commit SHA via `git rev-parse --verify`.
func revParse(ctx context.Context, dir, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", ref) //nolint:gosec // G204: fixed subcommand, dir/ref are caller-controlled
	cmd.Env = gitSubprocessEnv()
	cmd.WaitDelay = gitWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// archivePluginsTree extracts the named plugins/<name> directories at ref
// from gitDir's object store into destDir, via `git archive | tar -x` — no
// working tree or checkout involved.
func archivePluginsTree(ctx context.Context, gitDir, ref, destDir string, pluginNames []string) error {
	args := []string{"-C", gitDir, "archive", ref, "--"}
	for _, name := range pluginNames {
		args = append(args, filepath.Join("plugins", name))
	}
	gitCmd := exec.CommandContext(ctx, "git", args...)             //nolint:gosec // G204: fixed subcommand, args are caller-controlled plugin names
	tarCmd := exec.CommandContext(ctx, "tar", "-x", "-C", destDir) //nolint:gosec // G204: fixed args
	gitCmd.Env = gitSubprocessEnv()
	gitCmd.WaitDelay = gitWaitDelay
	tarCmd.WaitDelay = gitWaitDelay

	pipe, err := gitCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating pipe: %w", err)
	}
	tarCmd.Stdin = pipe

	var gitErr, tarErr bytes.Buffer
	gitCmd.Stderr = &gitErr
	tarCmd.Stderr = &tarErr

	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("starting tar: %w", err)
	}
	gitRunErr := gitCmd.Run()
	// Always wait on tar, even when git archive failed: git holds tar's
	// stdin pipe, so tar sees EOF and exits once git exits either way.
	// Skipping this on the error path leaked a zombie tar process on every
	// failed sync tick in the long-lived daemon (PR #236 review).
	tarWaitErr := tarCmd.Wait()
	if gitRunErr != nil {
		return fmt.Errorf("git archive: %w: %s", gitRunErr, strings.TrimSpace(gitErr.String()))
	}
	if tarWaitErr != nil {
		return fmt.Errorf("tar extract: %w: %s", tarWaitErr, strings.TrimSpace(tarErr.String()))
	}
	return nil
}

func hasPlugins(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			if _, err := os.Stat(filepath.Join(dir, entry.Name(), "plugin.md")); err == nil {
				return true
			}
		}
	}
	return false
}

// DriftReport describes differences between source and runtime plugins.
type DriftReport struct {
	Source  string       `json:"source"`
	Target  string       `json:"target"`
	Drifted []DriftEntry `json:"drifted,omitempty"`
	Missing []string     `json:"missing,omitempty"` // in source but not target
	Extra   []string     `json:"extra,omitempty"`   // in target but not source
}

// DriftEntry describes a single plugin that differs between source and runtime.
type DriftEntry struct {
	Name       string `json:"name"`
	SourceHash string `json:"source_hash"`
	TargetHash string `json:"target_hash"`
}

// DetectDrift compares plugin directories between source and target.
func DetectDrift(sourceDir, targetDir string) (*DriftReport, error) {
	report := &DriftReport{
		Source: sourceDir,
		Target: targetDir,
	}

	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("reading source: %w", err)
	}

	tgtPlugins := make(map[string]bool)
	if tgtEntries, err := os.ReadDir(targetDir); err == nil {
		for _, entry := range tgtEntries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				tgtPlugins[entry.Name()] = true
			}
		}
	}

	for _, entry := range srcEntries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(sourceDir, entry.Name(), "plugin.md")); err != nil {
			continue
		}

		srcDir := filepath.Join(sourceDir, entry.Name())
		dstDir := filepath.Join(targetDir, entry.Name())

		if !tgtPlugins[entry.Name()] {
			report.Missing = append(report.Missing, entry.Name())
			continue
		}
		delete(tgtPlugins, entry.Name())

		srcHash := DirHash(srcDir)
		dstHash := DirHash(dstDir)
		if srcHash != dstHash {
			report.Drifted = append(report.Drifted, DriftEntry{
				Name:       entry.Name(),
				SourceHash: srcHash,
				TargetHash: dstHash,
			})
		}
	}

	for name := range tgtPlugins {
		if _, err := os.Stat(filepath.Join(targetDir, name, "plugin.md")); err == nil {
			report.Extra = append(report.Extra, name)
		}
	}

	return report, nil
}

// HasDrift returns true if the report indicates any differences.
func (r *DriftReport) HasDrift() bool {
	return len(r.Drifted) > 0 || len(r.Missing) > 0
}
