package rig

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestCopyOverlay_NoOverlayDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := t.TempDir()

	// No overlay directory exists
	err := CopyOverlay(tmpDir, destDir)
	if err != nil {
		t.Errorf("CopyOverlay() with no overlay directory should return nil, got %v", err)
	}
}

func TestCopyOverlay_CopiesFiles(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with test files
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	// Create test files
	testFile1 := filepath.Join(overlayDir, "test1.txt")
	testFile2 := filepath.Join(overlayDir, "test2.txt")

	if err := os.WriteFile(testFile1, []byte("content1"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	if err := os.WriteFile(testFile2, []byte("content2"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify files were copied
	destFile1 := filepath.Join(destDir, "test1.txt")
	destFile2 := filepath.Join(destDir, "test2.txt")

	content1, err := os.ReadFile(destFile1)
	if err != nil {
		t.Errorf("File test1.txt was not copied: %v", err)
	}
	if string(content1) != "content1" {
		t.Errorf("test1.txt content = %q, want %q", string(content1), "content1")
	}

	content2, err := os.ReadFile(destFile2)
	if err != nil {
		t.Errorf("File test2.txt was not copied: %v", err)
	}
	if string(content2) != "content2" {
		t.Errorf("test2.txt content = %q, want %q", string(content2), "content2")
	}
}

func TestCopyOverlay_PreservesPermissions(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with a file
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	testFile := filepath.Join(overlayDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0755); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify permissions were preserved
	srcInfo, _ := os.Stat(testFile)
	destInfo, err := os.Stat(filepath.Join(destDir, "test.txt"))
	if err != nil {
		t.Fatalf("Failed to stat destination file: %v", err)
	}

	if srcInfo.Mode().Perm() != destInfo.Mode().Perm() {
		t.Errorf("Permissions not preserved: src=%v, dest=%v", srcInfo.Mode(), destInfo.Mode())
	}
}

func TestCopyOverlay_SkipsSubdirectories(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with a subdirectory
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	// Create a subdirectory
	subDir := filepath.Join(overlayDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	// Create a file in the overlay root
	testFile := filepath.Join(overlayDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create a file in the subdirectory
	subFile := filepath.Join(subDir, "sub.txt")
	if err := os.WriteFile(subFile, []byte("subcontent"), 0644); err != nil {
		t.Fatalf("Failed to create sub file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify root file was copied
	if _, err := os.Stat(filepath.Join(destDir, "test.txt")); err != nil {
		t.Error("Root file should be copied")
	}

	// Verify subdirectory was NOT copied
	if _, err := os.Stat(filepath.Join(destDir, "subdir")); err == nil {
		t.Error("Subdirectory should not be copied")
	}
	if _, err := os.Stat(filepath.Join(destDir, "subdir", "sub.txt")); err == nil {
		t.Error("File in subdirectory should not be copied")
	}
}

func TestCopyOverlay_EmptyOverlay(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create empty overlay directory
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Should succeed without errors
}

func TestCopyOverlay_OverwritesExisting(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with test file
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	testFile := filepath.Join(overlayDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("new content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create existing file in destination with different content
	destFile := filepath.Join(destDir, "test.txt")
	if err := os.WriteFile(destFile, []byte("old content"), 0644); err != nil {
		t.Fatalf("Failed to create dest file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify file was overwritten
	content, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("Failed to read dest file: %v", err)
	}
	if string(content) != "new content" {
		t.Errorf("File content = %q, want %q", string(content), "new content")
	}
}

func TestCopyFilePreserveMode(t *testing.T) {
	tmpDir := t.TempDir()

	// Create source file
	srcFile := filepath.Join(tmpDir, "src.txt")
	if err := os.WriteFile(srcFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create src file: %v", err)
	}

	// Copy file
	dstFile := filepath.Join(tmpDir, "dst.txt")
	err := copyFilePreserveMode(srcFile, dstFile)
	if err != nil {
		t.Fatalf("copyFilePreserveMode() error = %v", err)
	}

	// Verify content
	content, err := os.ReadFile(dstFile)
	if err != nil {
		t.Errorf("Failed to read dst file: %v", err)
	}
	if string(content) != "test content" {
		t.Errorf("Content = %q, want %q", string(content), "test content")
	}

	// Verify permissions
	srcInfo, _ := os.Stat(srcFile)
	dstInfo, err := os.Stat(dstFile)
	if err != nil {
		t.Fatalf("Failed to stat dst file: %v", err)
	}
	if srcInfo.Mode().Perm() != dstInfo.Mode().Perm() {
		t.Errorf("Permissions not preserved: src=%v, dest=%v", srcInfo.Mode(), dstInfo.Mode())
	}
}

func TestCopyFilePreserveMode_NonexistentSource(t *testing.T) {
	tmpDir := t.TempDir()

	srcFile := filepath.Join(tmpDir, "nonexistent.txt")
	dstFile := filepath.Join(tmpDir, "dst.txt")

	err := copyFilePreserveMode(srcFile, dstFile)
	if err == nil {
		t.Error("copyFilePreserveMode() with nonexistent source should return error")
	}
}

func TestEnsureGitignorePatterns_CreatesNewFile(t *testing.T) {
	tmpDir := t.TempDir()

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Check all required patterns are present (.beads/ intentionally excluded — see overlay.go)
	patterns := []string{".runtime/", ".claude/", ".opencode/", ".logs/", "__pycache__/", "state.json"}
	for _, pattern := range patterns {
		if !containsLine(string(content), pattern) {
			t.Errorf(".gitignore missing pattern %q", pattern)
		}
	}
}

func TestEnsureGitignorePatterns_AppendsToExisting(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with some content
	existing := "node_modules/\n*.log\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Should preserve existing content
	if !containsLine(string(content), "node_modules/") {
		t.Error("Existing pattern node_modules/ was removed")
	}

	// Should add header
	if !containsLine(string(content), "# Gas Town (added by gt)") {
		t.Error("Missing Gas Town header comment")
	}

	// Should add required patterns (.beads/ intentionally excluded — see overlay.go)
	patterns := []string{".runtime/", ".claude/", ".opencode/", ".logs/", "__pycache__/", "state.json"}
	for _, pattern := range patterns {
		if !containsLine(string(content), pattern) {
			t.Errorf(".gitignore missing pattern %q", pattern)
		}
	}
}

func TestEnsureGitignorePatterns_SkipsExistingPatterns(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with some Gas Town patterns already.
	// The broader ".claude/" covers ".claude/commands/", so it should
	// not add the narrower pattern.
	existing := ".runtime/\n.claude/\n.opencode/\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Should not duplicate existing patterns
	count := countOccurrences(string(content), ".runtime/")
	if count != 1 {
		t.Errorf(".runtime/ appears %d times, expected 1", count)
	}

	// .claude/ is now a direct required pattern — should not be duplicated
	claudeCount := countOccurrences(string(content), ".claude/")
	if claudeCount != 1 {
		t.Errorf(".claude/ appears %d times, expected 1", claudeCount)
	}
	opencodeCount := countOccurrences(string(content), ".opencode/")
	if opencodeCount != 1 {
		t.Errorf(".opencode/ appears %d times, expected 1", opencodeCount)
	}

	// Should add missing patterns
	if !containsLine(string(content), ".logs/") {
		t.Error(".gitignore missing pattern .logs/")
	}
	if !containsLine(string(content), "__pycache__/") {
		t.Error(".gitignore missing pattern __pycache__/")
	}
	if !containsLine(string(content), "state.json") {
		t.Error(".gitignore missing pattern state.json")
	}

	// Regression guard: .beads/ must NOT be in required patterns.
	// Beads manages its own .beads/.gitignore via bd init.
	// Adding .beads/ here breaks bd sync. This has regressed twice
	// (PR #753, #966). If this test fails, you're about to break polecats.
	if containsLine(string(content), ".beads/") {
		t.Error(".gitignore must NOT contain .beads/ - beads manages its own .gitignore (see overlay.go comment)")
	}
}

func TestEnsureGitignorePatterns_RecognizesVariants(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with variant patterns (without trailing slash).
	// ".claude" (no trailing slash) should be recognized as covering ".claude/commands/".
	existing := ".runtime\n/.claude\n/.opencode\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Should recognize variants and not add duplicates
	// .runtime (no slash) should count as .runtime/
	runtimeCount := countOccurrences(string(content), ".runtime")
	if runtimeCount > 1 {
		t.Errorf(".runtime appears %d times (variant detection failed)", runtimeCount)
	}

	// /.claude (leading slash, no trailing slash) should cover .claude/
	if containsLine(string(content), ".claude/") {
		t.Error(".claude/ should not be added when /.claude already covers it")
	}
	if containsLine(string(content), ".opencode/") {
		t.Error(".opencode/ should not be added when /.opencode already covers it")
	}
}

func TestEnsureGitignorePatterns_AllPatternsPresent(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with all required patterns.
	existing := ".runtime/\n.claude/\n.opencode/\n.beads/\n.logs/\n__pycache__/\nstate.json\nCLAUDE.md\nCLAUDE.local.md\nGEMINI.md\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// File should be unchanged (no header added)
	if containsLine(string(content), "# Gas Town") {
		t.Error("Should not add header when all patterns already present")
	}

	// Content should match original
	if string(content) != existing {
		t.Errorf("File was modified when it shouldn't be.\nGot: %q\nWant: %q", string(content), existing)
	}
}

func TestEnsureGitignorePatterns_NarrowPatternPresent(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .gitignore with the exact required patterns
	existing := ".runtime/\n.claude/\n.opencode/\n.logs/\n__pycache__/\nstate.json\nCLAUDE.md\nCLAUDE.local.md\nGEMINI.md\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// File should be unchanged
	if string(content) != existing {
		t.Errorf("File was modified when it shouldn't be.\nGot: %q\nWant: %q", string(content), existing)
	}
}

func TestEnsureGitignorePatterns_OldNarrowClaudeUpgraded(t *testing.T) {
	tmpDir := t.TempDir()

	// Simulate old installation with narrow .claude/commands/ pattern.
	// After upgrade, .claude/ (broad) should be added since .claude/commands/
	// does NOT cover .claude/ (the narrow is a subset, not a superset).
	existing := ".runtime/\n.claude/commands/\n.logs/\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// .claude/ should be added (old .claude/commands/ doesn't cover it)
	if !containsLine(string(content), ".claude/") {
		t.Error(".claude/ should be added when only .claude/commands/ was present")
	}

	// __pycache__/ should be added
	if !containsLine(string(content), "__pycache__/") {
		t.Error("__pycache__/ should be added")
	}
}

func TestEnsureGitignorePatterns_UpgradePreservesBroadPattern(t *testing.T) {
	tmpDir := t.TempDir()

	// Simulate an existing installation that has .claude/ plus other Gas Town
	// patterns but is missing __pycache__/ (added later). After upgrade,
	// __pycache__/ should be appended.
	existing := "# Gas Town (added by gt)\n.runtime/\n.claude/\n.logs/\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// __pycache__/ should be appended
	if !containsLine(string(content), "__pycache__/") {
		t.Error("__pycache__/ should be added during upgrade")
	}

	// Existing patterns should be preserved
	if !containsLine(string(content), ".runtime/") {
		t.Error(".runtime/ should be preserved")
	}
	if !containsLine(string(content), ".claude/") {
		t.Error(".claude/ should be preserved")
	}
}

// TestGasTownLocalExcludePatterns_IncludesBeads verifies that the local exclude
// patterns include .beads/ (defense-in-depth for gas-7vg) while the gitignore
// patterns do NOT include .beads/ (regression guard).
func TestGasTownLocalExcludePatterns_IncludesBeads(t *testing.T) {
	localPatterns := gasTownLocalExcludePatterns()
	found := false
	for _, p := range localPatterns {
		if p == "/.beads/" {
			found = true
			break
		}
	}
	if !found {
		t.Error("gasTownLocalExcludePatterns() must include /.beads/ (gas-7vg defense-in-depth)")
	}

	// Regression guard: .gitignore patterns must NOT include .beads/
	gitignorePatterns := gasTownIgnorePatterns()
	for _, p := range gitignorePatterns {
		if p == ".beads/" {
			t.Error("gasTownIgnorePatterns() must NOT include .beads/ - that breaks bd sync (see overlay.go)")
		}
	}
}

// Helper functions

func containsLine(content, pattern string) bool {
	for _, line := range splitLines(content) {
		if line == pattern {
			return true
		}
	}
	return false
}

func countOccurrences(content, pattern string) int {
	count := 0
	for _, line := range splitLines(content) {
		if line == pattern {
			count++
		}
	}
	return count
}

func splitLines(content string) []string {
	var lines []string
	start := 0
	for i, c := range content {
		if c == '\n' {
			lines = append(lines, content[start:i])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// Polecat worktrees are linked worktrees of a shared bare repo. git reads
// info/exclude only from the shared git dir, so the excludes must reach git through
// a per-worktree core.excludesFile, and enabling per-worktree config must not break
// the bare repo or the sibling worktrees that share it.
func TestEnsureLocalExcludePatterns_LinkedWorktreeOfBareRepo(t *testing.T) {
	isolateGitConfig(t, "")
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	git(src, "init", "-q")
	git(src, "commit", "-q", "--allow-empty", "-m", "init")
	bare := filepath.Join(root, "repo.git")
	git(root, "clone", "-q", "--bare", src, bare)
	polecat := filepath.Join(root, "polecat")
	sibling := filepath.Join(root, "sibling")
	git(bare, "worktree", "add", "-q", polecat, "-b", "p")
	git(bare, "worktree", "add", "-q", sibling, "-b", "s")

	provisioned := []string{".runtime/session_id", ".beads/x.jsonl", "CLAUDE.local.md", ".claude/commands/" + gasTownCommandName(t) + ".md"}
	work := []string{"notes.txt", ".claude/skills/new/SKILL.md", "CLAUDE.md", "docs/CLAUDE.local.md"}
	for _, wt := range []string{polecat, sibling} {
		for _, rel := range append(append([]string{}, provisioned...), work...) {
			path := filepath.Join(wt, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	for i := 0; i < 2; i++ { // second call must be a no-op
		if err := EnsureLocalExcludePatterns(polecat); err != nil {
			t.Fatalf("EnsureLocalExcludePatterns (call %d): %v", i+1, err)
		}
	}

	untracked := func(wt string) string {
		return strings.Join(strings.Fields(strings.ReplaceAll(git(wt, "status", "--porcelain", "-uall"), "?? ", "")), " ")
	}
	if got, want := untracked(polecat), ".claude/skills/new/SKILL.md CLAUDE.md docs/CLAUDE.local.md notes.txt"; got != want {
		t.Errorf("polecat worktree untracked = %q, want only real work %q", got, want)
	}
	// The sibling shares the repo but was not set up: nothing may be hidden there.
	for _, rel := range provisioned {
		if !strings.Contains(untracked(sibling), rel) {
			t.Errorf("sibling worktree lost %s from git status; excludes leaked across worktrees", rel)
		}
	}
	// The shared bare repo must still be bare and usable.
	if got := git(bare, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("bare repo is-bare = %q, want true", got)
	}
	git(bare, "worktree", "add", "-q", filepath.Join(root, "third"), "-b", "t")
	git(sibling, "commit", "-q", "--allow-empty", "-m", "still works")
}

func gasTownCommandName(t *testing.T) string {
	t.Helper()
	for _, p := range gasTownLocalExcludePatterns() {
		if name, ok := strings.CutPrefix(p, "/.claude/commands/"); ok {
			return strings.TrimSuffix(name, ".md")
		}
	}
	t.Fatal("no provisioned command pattern")
	return ""
}

// isolateGitConfig hides the developer's global and system git config from the test
// and installs globalIgnore (if any) as git's default global excludes file.
func isolateGitConfig(t *testing.T, globalIgnore string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if globalIgnore != "" {
		if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(xdg, "git", "ignore"), []byte(globalIgnore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// bareRepoWithWorktrees creates a bare repo under a temp dir with n linked worktrees.
func bareRepoWithWorktrees(t *testing.T, n int) (bare string, worktrees []string) {
	t.Helper()
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	run(src, "init", "-q")
	run(src, "commit", "-q", "--allow-empty", "-m", "init")
	bare = filepath.Join(root, "repo.git")
	run(root, "clone", "-q", "--bare", src, bare)
	for i := 0; i < n; i++ {
		wt := filepath.Join(root, fmt.Sprintf("wt%d", i))
		run(bare, "worktree", "add", "-q", wt, "-b", fmt.Sprintf("b%d", i))
		worktrees = append(worktrees, wt)
	}
	return bare, worktrees
}

// git reads one excludes file, so the per-worktree file must carry along whatever
// excludes file applied before. Otherwise a secret hidden only by the user's ignore
// file becomes visible to `git add -A` in every polecat.
func TestEnsureLocalExcludePatterns_KeepsInheritedIgnore(t *testing.T) {
	ignoreFile := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "myignore")
		if err := os.WriteFile(path, []byte(".env\n*.pem\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeConfig := func(t *testing.T, path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]func(t *testing.T, bare string){
		"default XDG ignore file": func(t *testing.T, _ string) {
			isolateGitConfig(t, ".env\n*.pem\n")
		},
		"core.excludesFile in global config": func(t *testing.T, _ string) {
			isolateGitConfig(t, "")
			global := filepath.Join(t.TempDir(), "gitconfig")
			writeConfig(t, global, "[core]\n\texcludesFile = "+filepath.ToSlash(ignoreFile(t))+"\n")
			t.Setenv("GIT_CONFIG_GLOBAL", global)
		},
		"core.excludesFile via include in global config": func(t *testing.T, _ string) {
			isolateGitConfig(t, "")
			dir := t.TempDir()
			included := filepath.Join(dir, "gitconfig.local")
			writeConfig(t, included, "[core]\n\texcludesFile = "+filepath.ToSlash(ignoreFile(t))+"\n")
			global := filepath.Join(dir, "gitconfig")
			writeConfig(t, global, "[include]\n\tpath = "+filepath.ToSlash(included)+"\n")
			t.Setenv("GIT_CONFIG_GLOBAL", global)
		},
		"core.excludesFile in repository config": func(t *testing.T, bare string) {
			isolateGitConfig(t, "")
			if out, err := exec.Command("git", "--git-dir", bare, "config", "core.excludesFile", ignoreFile(t)).CombinedOutput(); err != nil {
				t.Fatalf("git config: %v\n%s", err, out)
			}
		},
	}
	tests["relative core.excludesFile in repository config"] = func(t *testing.T, bare string) {
		isolateGitConfig(t, "")
		if out, err := exec.Command("git", "--git-dir", bare, "config", "core.excludesFile", ".git-local-ignore").CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			isolateGitConfig(t, "") // for the repo setup; the case re-isolates as it needs
			bare, wts := bareRepoWithWorktrees(t, 1)
			setup(t, bare)
			for _, f := range []string{".env", "id.pem", "notes.txt"} {
				if err := os.WriteFile(filepath.Join(wts[0], f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Target of the relative case; it hides itself so every case sees the same status.
			if err := os.WriteFile(filepath.Join(wts[0], ".git-local-ignore"), []byte(".env\n*.pem\n.git-local-ignore\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(name, "relative") {
				if err := os.Remove(filepath.Join(wts[0], ".git-local-ignore")); err != nil {
					t.Fatal(err)
				}
			}
			status := func() string {
				out, err := exec.Command("git", "-C", wts[0], "status", "--porcelain", "-uall").CombinedOutput()
				if err != nil {
					t.Fatalf("git status: %v\n%s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			if got := status(); got != "?? notes.txt" {
				t.Fatalf("precondition: status before setup = %q, want only notes.txt", got)
			}
			for i := 0; i < 2; i++ { // the re-run must not lose the inherited patterns
				if err := EnsureLocalExcludePatterns(wts[0]); err != nil {
					t.Fatalf("EnsureLocalExcludePatterns: %v", err)
				}
				if got := status(); got != "?? notes.txt" {
					t.Errorf("after call %d: status = %q, want only notes.txt (inherited ignores must stay in force)", i+1, got)
				}
			}
		})
	}
}

// A repository can switch the global ignore off with an empty core.excludesFile.
// Files it left visible on purpose must not be hidden again by a copied-in ignore.
func TestEnsureLocalExcludePatterns_EmptyExcludesFileInheritsNothing(t *testing.T) {
	isolateGitConfig(t, ".env\n")
	bare, wts := bareRepoWithWorktrees(t, 1)
	if out, err := exec.Command("git", "--git-dir", bare, "config", "core.excludesFile", "").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(wts[0], ".env"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalExcludePatterns(wts[0]); err != nil {
		t.Fatalf("EnsureLocalExcludePatterns: %v", err)
	}
	out, _ := exec.Command("git", "-C", wts[0], "status", "--porcelain", "-uall").CombinedOutput()
	if got := strings.TrimSpace(string(out)); got != "?? .env" {
		t.Errorf("status = %q, want .env still visible", got)
	}
}

// If git cannot report the inherited excludes file (git older than 2.26 rejects
// --show-scope), installing the per-worktree file anyway would un-ignore whatever
// the user's own ignore hides. The setup must fail and leave the worktree alone.
func TestEnsureLocalExcludePatterns_FailsClosedWhenGitCannotResolveInherited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a sh git wrapper")
	}
	isolateGitConfig(t, "")
	_, wts := bareRepoWithWorktrees(t, 1)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --show-scope ] && { echo 'error: unknown option' >&2; exit 129; }; done\nexec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = EnsureLocalExcludePatterns(wts[0])
	if err == nil {
		t.Fatal("EnsureLocalExcludePatterns succeeded although the inherited excludes could not be resolved")
	}
	if !strings.Contains(err.Error(), "unknown option") || !strings.Contains(err.Error(), "2.26") {
		t.Errorf("error %q should carry git's stderr and the minimum git version", err)
	}
	out, _ := exec.Command(realGit, "-C", wts[0], "config", "--get", "core.excludesFile").Output()
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("core.excludesFile = %q, want unset after a failed setup", got)
	}
}

// Sibling polecats are spawned in parallel under per-polecat locks only, so the
// one-time repository migration must be safe to race.
func TestEnsureLocalExcludePatterns_ConcurrentFirstEnable(t *testing.T) {
	isolateGitConfig(t, "")
	bare, wts := bareRepoWithWorktrees(t, 8)
	errs := make([]error, len(wts))
	var wg sync.WaitGroup
	for i, wt := range wts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = EnsureLocalExcludePatterns(wt)
		}()
	}
	wg.Wait()
	for i, wt := range wts {
		if errs[i] != nil {
			t.Errorf("worktree %d: %v", i, errs[i])
		}
		out, _ := exec.Command("git", "-C", wt, "config", "--worktree", "--get", "core.excludesFile").Output()
		if strings.TrimSpace(string(out)) == "" {
			t.Errorf("worktree %d has no core.excludesFile", i)
		}
	}
	out, err := exec.Command("git", "--git-dir", bare, "rev-parse", "--is-bare-repository").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Errorf("bare repo is-bare = %q (%v), want true", strings.TrimSpace(string(out)), err)
	}
}
