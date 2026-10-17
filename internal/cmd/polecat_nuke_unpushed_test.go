package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// nukeFixture builds a rig with one polecat worktree carrying one commit that
// exists nowhere else, on a repo whose origin cannot be pushed to.
func nukeFixture(t *testing.T) (*polecat.Manager, *rig.Rig, string) {
	t.Helper()
	root := t.TempDir()
	mayor := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayor, 0755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, mayor, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(mayor, "README.md"), []byte("# t\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, mayor, "add", "README.md")
	runGitT(t, mayor, "commit", "-m", "init")
	runGitT(t, mayor, "remote", "add", "origin", filepath.Join(root, "does-not-exist.git"))
	runGitT(t, mayor, "update-ref", "refs/remotes/origin/main", "HEAD")

	clone := filepath.Join(root, "polecats", "toast", "rig")
	if err := os.MkdirAll(filepath.Dir(clone), 0755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, mayor, "worktree", "add", "-b", "polecat/toast-x", clone, "main")
	if err := os.WriteFile(filepath.Join(clone, "real-work.txt"), []byte("work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, clone, "add", "real-work.txt")
	runGitT(t, clone, "commit", "-m", "genuinely unpushed work")

	r := &rig.Rig{Name: "rig", Path: root}
	return polecat.NewManager(r, git.NewGit(root), nil), r, clone
}

// TestNukeRefusesUnpushedCommitsBeforeAnyDestructiveStep pins gt-yu9l: nuke
// aborts on commits that exist only in the worktree, before it kills the
// session or touches the worktree, and --force alone does not change that.
func TestNukeRefusesUnpushedCommitsBeforeAnyDestructiveStep(t *testing.T) {
	mgr, r, clone := nukeFixture(t)

	err := nukePolecatFullWithOptions("toast", "rig", mgr, r, nukePolecatOptions{})
	var risk *polecat.WorkAtRiskError
	if !errors.As(err, &risk) {
		t.Fatalf("nuke err = %v, want *polecat.WorkAtRiskError", err)
	}
	// "worktree removal failed" means the refusal came from the removal step,
	// i.e. after the session was already killed and molecules burned.
	if strings.Contains(err.Error(), "worktree removal failed") {
		t.Fatalf("refusal came from the removal step, not the preflight: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(clone, "real-work.txt")); statErr != nil {
		t.Fatalf("worktree content gone after a refused nuke: %v", statErr)
	}
}

// TestNukeRefusesUnpushedDetachedCommitBeforeAnyDestructiveStep pins that the
// nuke preflight holds a detached HEAD to the nuclear standard the removal
// step will apply, so the refusal still comes before the session is killed.
func TestNukeRefusesUnpushedDetachedCommitBeforeAnyDestructiveStep(t *testing.T) {
	mgr, r, clone := nukeFixture(t)
	runGitT(t, clone, "checkout", "--detach")

	err := nukePolecatFullWithOptions("toast", "rig", mgr, r, nukePolecatOptions{})
	var risk *polecat.WorkAtRiskError
	if !errors.As(err, &risk) {
		t.Fatalf("nuke err = %v, want *polecat.WorkAtRiskError", err)
	}
	if strings.Contains(err.Error(), "worktree removal failed") {
		t.Fatalf("refusal came from the removal step, not the preflight: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(clone, "real-work.txt")); statErr != nil {
		t.Fatalf("worktree content gone after a refused nuke: %v", statErr)
	}
}

func TestNukeDiscardUnpushedIsASeparateFlagFromForce(t *testing.T) {
	f := polecatNukeCmd.Flags()
	if f.Lookup("discard-unpushed") == nil {
		t.Fatal("gt polecat nuke has no --discard-unpushed flag")
	}
	if f.Lookup("force") == nil {
		t.Fatal("gt polecat nuke lost --force")
	}
}
