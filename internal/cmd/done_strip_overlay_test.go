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
