package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// trackedConfigTown builds a town whose rig .beads/config.yaml is committed to
// a git repo (as it is in every gastown worktree) and lacks the Dolt keys.
func trackedConfigTown(t *testing.T, track bool) (townRoot, configPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	townRoot = t.TempDir()
	repo := filepath.Join(townRoot, "gastown", "polecats", "guzzle", "gastown")
	beadsDir := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("prefix: gt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	meta := `{"dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"gastown"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if track {
		git("add", ".beads/config.yaml")
		git("commit", "-q", "-m", "init")
	}
	return townRoot, configPath
}

// gt doctor --fix runs unattended (dogs, mol-session-gc) in worktrees whose
// config.yaml is committed. Writing the Dolt keys there dirties the tree and
// auto-save sweeps them into an unrelated polecat's MR (gt-pyrkd, gt-15qi).
func TestDoltConfigCheck_FixNeverModifiesTrackedConfig(t *testing.T) {
	townRoot, configPath := trackedConfigTown(t, true)

	check := NewDoltConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	if err := check.Fix(ctx); err == nil {
		t.Fatal("Fix() = nil, want an error naming the tracked config it declined to edit")
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "prefix: gt\n" {
		t.Fatalf("tracked config.yaml was modified by Fix:\n%s", got)
	}
}

// An untracked config.yaml (not in a git worktree, or ignored) is still fixed.
func TestDoltConfigCheck_FixStillEditsUntrackedConfig(t *testing.T) {
	townRoot, configPath := trackedConfigTown(t, false)

	check := NewDoltConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("Status = %v, want %v", result.Status, StatusError)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() error = %v", err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "prefix: gt\n" {
		t.Fatal("untracked config.yaml was not fixed")
	}
}

// The auto-start repair rewrites config.yaml too, so it gets the same guard.
func TestAutoStartConfigCheck_FixNeverModifiesTrackedConfig(t *testing.T) {
	townRoot, configPath := trackedConfigTown(t, true)

	check := NewAutoStartConfigCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	if result := check.Run(ctx); result.Status == StatusOK {
		t.Fatalf("Run() = OK, want the unpinned store flagged")
	}
	if err := check.Fix(ctx); err == nil {
		t.Fatal("Fix() = nil, want an error naming the tracked config it declined to edit")
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "prefix: gt\n" {
		t.Fatalf("tracked config.yaml was modified by Fix:\n%s", got)
	}
}
