package rig

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates/commands"
)

func gasTownIgnorePatterns() []string {
	return []string{
		".runtime/",
		".claude/",
		".opencode/",
		".logs/",
		"__pycache__/",
		"state.json",
		"CLAUDE.md",
		"CLAUDE.local.md",
		"GEMINI.md",
	}
}

// CopyOverlay copies files from <rigPath>/.runtime/overlay/ to the destination path.
// This allows storing gitignored files (like .env) that services need at their root.
// The overlay is copied non-recursively - only files, not subdirectories.
// File permissions from the source are preserved.
//
// Structure:
//
//	rig/
//	  .runtime/
//	    overlay/
//	      .env          <- Copied to destPath
//	      config.json   <- Copied to destPath
//
// Returns nil if the overlay directory doesn't exist (nothing to copy).
// Individual file copy failures are logged as warnings but don't stop the process.
func CopyOverlay(rigPath, destPath string) error {
	overlayDir := filepath.Join(rigPath, ".runtime", "overlay")

	// Check if overlay directory exists
	entries, err := os.ReadDir(overlayDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No overlay directory - not an error, just nothing to copy
			return nil
		}
		return fmt.Errorf("reading overlay dir: %w", err)
	}

	// Copy each file (not directories) from overlay to destination
	for _, entry := range entries {
		if entry.IsDir() {
			// Skip subdirectories - only copy files at overlay root
			continue
		}

		srcPath := filepath.Join(overlayDir, entry.Name())
		dstPath := filepath.Join(destPath, entry.Name())

		if err := copyFilePreserveMode(srcPath, dstPath); err != nil {
			// Log warning but continue - don't fail spawn for overlay issues
			style.PrintWarning("could not copy overlay file %s: %v", entry.Name(), err)
			continue
		}
	}

	return nil
}

// EnsureGitignorePatterns ensures the .gitignore has required Gas Town patterns.
// This is called after cloning to add patterns that may be missing from the source repo.
func EnsureGitignorePatterns(worktreePath string) error {
	gitignorePath := filepath.Join(worktreePath, ".gitignore")

	// Required patterns for Gas Town worktrees.
	// DO NOT add ".beads/" here. Beads manages its own .beads/.gitignore
	// (created by bd init) which selectively ignores runtime files.
	// Adding .beads/ here overrides that and breaks bd sync.
	// This has regressed twice (PR #753 added it, #891 removed it,
	// #966 re-added it). See overlay_test.go for a regression guard.
	//
	// .claude/ is the broad pattern (covers commands/, settings.json, rules/, etc.).
	// Settings are installed in gastown-managed parent directories via --settings flag,
	// but Cursor still creates .claude/ inside worktrees at runtime. The narrow
	// .claude/commands/ pattern missed other Cursor-created files, causing gt done
	// to fail with "uncommitted changes would be lost" on untracked .claude/ entries.
	requiredPatterns := gasTownIgnorePatterns()

	// Read existing gitignore content
	var existingContent string
	if data, err := os.ReadFile(gitignorePath); err == nil {
		existingContent = string(data)
	}

	// Find missing patterns
	var missing []string
	for _, pattern := range requiredPatterns {
		found := false
		for _, line := range strings.Split(existingContent, "\n") {
			line = strings.TrimSpace(line)
			if matchesGitignorePattern(line, pattern) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, pattern)
		}
	}

	if len(missing) == 0 {
		return nil // All patterns present
	}

	// Append missing patterns
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening .gitignore: %w", err)
	}
	defer f.Close()

	// Add header if appending to existing file
	if existingContent != "" && !strings.HasSuffix(existingContent, "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	if existingContent != "" {
		if _, err := f.WriteString("\n# Gas Town (added by gt)\n"); err != nil {
			return err
		}
	}

	for _, pattern := range missing {
		if _, err := f.WriteString(pattern + "\n"); err != nil {
			return err
		}
	}

	return nil
}

// gasTownLocalExcludePatterns returns the patterns for a worktree's own exclude
// file: only the paths Gas Town itself writes into a polecat worktree, anchored at
// the worktree root. It is deliberately narrower than gasTownIgnorePatterns(): an
// exclude hides untracked files from git status and git add, so a broad pattern
// such as .claude/ would make a polecat's new skill or CLAUDE.md silently
// uncommittable. .beads/ is safe here (unlike in .gitignore, where it breaks bd
// sync) because this file is per-worktree and never committed.
func gasTownLocalExcludePatterns() []string {
	patterns := []string{"/.runtime/", "/.beads/", "/CLAUDE.local.md"}
	for _, name := range commands.Names() {
		patterns = append(patterns, "/.claude/commands/"+name+".md")
	}
	return patterns
}

// localExcludeFileName is the per-worktree exclude file, kept in the worktree's own
// git dir. It is not info/exclude: git reads info/exclude only from the common
// (shared) git dir, so the copy Gas Town used to write into a linked worktree's git
// dir was never read.
const localExcludeFileName = "gt-exclude"

// EnsureLocalExcludePatterns makes git ignore the files Gas Town provisions in this
// worktree only, without touching a tracked .gitignore or the exclude file shared
// by every worktree of the repository. It writes the patterns to a file in the
// worktree's own git dir and points the worktree's core.excludesFile at it, which
// needs git's per-worktree config (extensions.worktreeConfig).
//
// A per-worktree core.excludesFile takes the place of the user's global one inside
// this worktree.
func EnsureLocalExcludePatterns(worktreePath string) error {
	gitDir, err := gitRevParse(worktreePath, "--absolute-git-dir")
	if err != nil {
		return err
	}
	excludePath := filepath.Join(gitDir, "info", localExcludeFileName)
	if err := os.MkdirAll(filepath.Dir(excludePath), 0755); err != nil {
		return fmt.Errorf("creating local exclude dir: %w", err)
	}
	content := "# Gas Town (written by gt; do not edit)\n" + strings.Join(gasTownLocalExcludePatterns(), "\n") + "\n"
	if err := os.WriteFile(excludePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing local exclude: %w", err)
	}

	if err := enableWorktreeConfig(worktreePath); err != nil {
		return err
	}
	if out, err := exec.Command("git", "-C", worktreePath, "config", "--worktree", "core.excludesFile", excludePath).CombinedOutput(); err != nil {
		return fmt.Errorf("setting worktree core.excludesFile: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// enableWorktreeConfig turns on extensions.worktreeConfig for the repository that
// worktreePath belongs to. Once the extension is on, every linked worktree reads
// the common config as its own, so a core.bare=true left there makes all of them
// fail with "this operation must be run in a work tree". As git's own
// sparse-checkout does, core.bare is therefore moved to the main worktree's
// config.worktree first; each step leaves the repository usable, so concurrent
// agents in sibling worktrees are never broken mid-way.
func enableWorktreeConfig(worktreePath string) error {
	commonDir, err := gitRevParse(worktreePath, "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	commonConfig := filepath.Join(commonDir, "config")
	get := func(key string) string {
		out, _ := exec.Command("git", "config", "--file", commonConfig, "--get", key).Output()
		return strings.TrimSpace(string(out))
	}
	if get("extensions.worktreeConfig") == "true" {
		return nil
	}
	if get("core.worktree") != "" {
		return fmt.Errorf("not enabling per-worktree git config: %s sets core.worktree", commonConfig)
	}
	set := func(file string, args ...string) error {
		out, err := exec.Command("git", append([]string{"config", "--file", file}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git config %s in %s: %w: %s", strings.Join(args, " "), file, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if bare := get("core.bare"); bare != "" {
		if err := set(filepath.Join(commonDir, "config.worktree"), "core.bare", bare); err != nil {
			return err
		}
		// A concurrent caller may already have removed it; the next read decides.
		_ = set(commonConfig, "--unset", "core.bare")
		if get("core.bare") != "" {
			return fmt.Errorf("could not move core.bare out of %s", commonConfig)
		}
	}
	return set(commonConfig, "extensions.worktreeConfig", "true")
}

func gitRevParse(worktreePath, arg string) (string, error) {
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", arg).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w: %s", arg, err, strings.TrimSpace(string(out)))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("git rev-parse %s: empty result for %s", arg, worktreePath)
	}
	return dir, nil
}

// matchesGitignorePattern checks if a gitignore line covers the required pattern.
// Handles variant forms (with/without trailing slash, leading slash) and recognizes
// that a broader directory pattern (e.g., ".claude/") covers more specific paths
// (e.g., ".claude/commands/").
func matchesGitignorePattern(line, pattern string) bool {
	// Strip leading slash for comparison
	normLine := strings.TrimPrefix(line, "/")
	normPattern := strings.TrimPrefix(pattern, "/")

	// Exact match or trailing-slash variants
	if normLine == normPattern ||
		normLine == strings.TrimSuffix(normPattern, "/") ||
		normLine+"/" == normPattern {
		return true
	}

	// A broader directory pattern covers more specific paths underneath it.
	// e.g., ".claude/" covers ".claude/commands/"
	if strings.HasSuffix(normLine, "/") && strings.HasPrefix(normPattern, normLine) {
		return true
	}
	// Also handle directory pattern without trailing slash
	if !strings.Contains(normLine, "/") && strings.HasPrefix(normPattern, normLine+"/") {
		return true
	}

	return false
}

// copyFilePreserveMode copies a file from src to dst, preserving the source file's permissions.
func copyFilePreserveMode(src, dst string) error {
	// Get source file info for permissions
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	// Create destination file with same permissions
	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}

	// Copy contents
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		return fmt.Errorf("copy contents: %w", err)
	}

	// Explicitly check Close() — on many filesystems, buffered data is flushed
	// at Close() time, so a full-disk error surfaces here, not during Write.
	if err := dstFile.Close(); err != nil {
		return fmt.Errorf("closing destination: %w", err)
	}

	return nil
}
