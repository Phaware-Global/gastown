package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/templates"
)

// overlayRepo builds a worktree whose origin/main tracks the given files, then
// switches to a polecat branch.
func overlayRepo(t *testing.T, tracked map[string]string) string {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	local := filepath.Join(tmp, "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, local, "init", "-b", "main")
	runGit(t, local, "config", "user.email", "polecat@test.com")
	runGit(t, local, "config", "user.name", "Polecat")
	tracked["README.md"] = "# Test\n"
	for name, content := range tracked {
		if err := os.WriteFile(filepath.Join(local, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, local, "add", ".")
	runGit(t, local, "commit", "-m", "init")
	runGit(t, local, "remote", "add", "origin", remote)
	runGit(t, local, "push", "-u", "origin", "main")
	runGit(t, local, "checkout", "-b", "polecat/test/task")
	return local
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A CLAUDE.local.md the target branch already tracks must be restored, never
// deleted: a deletion would be an unauthored change in an unrelated MR.
func TestStripOverlayCLAUDEmd_TrackedLocalFileRestoredNotDeleted(t *testing.T) {
	dir := overlayRepo(t, map[string]string{"CLAUDE.local.md": "repo-owned\n"})
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.local.md"), []byte("repo-owned\noverlay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "feat: real work")

	if !stripOverlayCLAUDEmd(git.NewGit(dir), "main") {
		t.Fatal("expected a cleanup commit")
	}

	if got := gitOut(t, dir, "diff", "--name-status", "origin/main", "HEAD"); got != "A\tfeature.txt" {
		t.Errorf("branch diff vs origin/main = %q, want only the added feature.txt", got)
	}
	if got := gitOut(t, dir, "log", "-1", "--format=%an"); got != "Gas Town" {
		t.Errorf("cleanup commit author = %q, want Gas Town (not the polecat)", got)
	}
	if got := gitOut(t, dir, "log", "-1", "--format=%s"); strings.Contains(got, "gt-p35") {
		t.Errorf("cleanup commit message %q must not carry the dead gt-p35 reference", got)
	}
}

// An overlay the target branch does not track is dropped from the branch.
func TestStripOverlayCLAUDEmd_UntrackedOverlayRemoved(t *testing.T) {
	dir := overlayRepo(t, map[string]string{})
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.local.md"), []byte("overlay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(templates.PolecatLifecycleMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-f", ".")
	runGit(t, dir, "commit", "-m", "feat: real work")

	if !stripOverlayCLAUDEmd(git.NewGit(dir), "main") {
		t.Fatal("expected a cleanup commit")
	}
	if got := gitOut(t, dir, "diff", "--name-only", "origin/main", "HEAD"); got != "" {
		t.Errorf("branch diff vs origin/main = %q, want empty", got)
	}
}

// No overlay files changed: nothing is committed.
func TestStripOverlayCLAUDEmd_NoopWithoutOverlay(t *testing.T) {
	dir := overlayRepo(t, map[string]string{})
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "feat: real work")
	if stripOverlayCLAUDEmd(git.NewGit(dir), "main") {
		t.Fatal("unexpected cleanup commit")
	}
}

// The cleanup commit holds only the overlay paths: an unrelated staged file
// stays staged and out of the commit.
func TestStripOverlayCLAUDEmd_LeavesUnrelatedStagedFilesStaged(t *testing.T) {
	dir := overlayRepo(t, map[string]string{})
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.local.md"), []byte("overlay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-f", "CLAUDE.local.md")
	runGit(t, dir, "commit", "-m", "feat: work")
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "staged.txt")

	if !stripOverlayCLAUDEmd(git.NewGit(dir), "main") {
		t.Fatal("expected a cleanup commit")
	}

	if got := gitOut(t, dir, "show", "--name-only", "--format=", "HEAD"); got != "CLAUDE.local.md" {
		t.Errorf("cleanup commit paths = %q, want only CLAUDE.local.md", got)
	}
	if got := gitOut(t, dir, "diff", "--cached", "--name-status"); got != "A\tstaged.txt" {
		t.Errorf("staged after cleanup = %q, want staged.txt still staged", got)
	}
	if got := gitOut(t, dir, "status", "--porcelain", "--", "CLAUDE.local.md"); strings.Contains(got, "D ") {
		t.Errorf("overlay path left a stale staged change: %q", got)
	}
}

// Restoring a tracked CLAUDE.local.md must not overwrite the polecat's
// on-disk overlay.
func TestStripOverlayCLAUDEmd_PreservesWorkingTreeOverlay(t *testing.T) {
	dir := overlayRepo(t, map[string]string{"CLAUDE.local.md": "repo-owned\n"})
	const overlay = "repo-owned\npolecat overlay\n"
	path := filepath.Join(dir, "CLAUDE.local.md")
	if err := os.WriteFile(path, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-am", "feat: work")

	if !stripOverlayCLAUDEmd(git.NewGit(dir), "main") {
		t.Fatal("expected a cleanup commit")
	}

	if got, _ := os.ReadFile(path); string(got) != overlay {
		t.Errorf("working-tree CLAUDE.local.md = %q, want the overlay preserved", got)
	}
	if got := gitOut(t, dir, "diff", "--name-only", "origin/main", "HEAD"); got != "" {
		t.Errorf("branch diff vs origin/main = %q, want empty", got)
	}
}
