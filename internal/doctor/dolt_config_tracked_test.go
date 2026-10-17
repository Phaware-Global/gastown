package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// Fix can never clear a tracked config, so Run must not report an error (which
	// would fail every unattended --fix) or point at --fix.
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want %v", result.Status, StatusWarning)
	}
	if strings.Contains(result.FixHint, "gt doctor --fix") {
		t.Errorf("FixHint = %q, must not point at --fix", result.FixHint)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() = %v, want nil: tracked configs are reported by Run, not Fix's problem", err)
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
	result := check.Run(ctx)
	if result.Status == StatusOK {
		t.Fatalf("Run() = OK, want the unpinned store flagged")
	}
	if check.CanFix() || strings.Contains(result.FixHint, "gt doctor --fix") {
		t.Errorf("tracked store reads as fixable: CanFix=%v hint=%q", check.CanFix(), result.FixHint)
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

// A branch can commit a fake gitdir (HEAD, config, index, objects/, refs/) next
// to a .beads dir. git adopts it as the repository and runs its core.fsmonitor
// command, so isGitTracked must neither run it nor trust its index.
func TestIsGitTracked_IgnoresNestedGitdirPayload(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Build a real repo whose index tracks .beads/config.yaml, then flatten its
	// gitdir into the worktree so the directory is its own fake gitdir.
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".beads", "config.yaml"), []byte("prefix: gt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(src, "init", "-q")
	run(src, "add", ".beads/config.yaml")
	run(src, "commit", "-q", "-m", "init")
	marker := filepath.Join(t.TempDir(), "ran")
	run(src, "config", "core.fsmonitor", "touch "+marker)
	run(src, "config", "core.worktree", "..")

	evil := t.TempDir()
	if err := os.CopyFS(evil, os.DirFS(filepath.Join(src, ".git"))); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(evil, ".beads"), os.DirFS(filepath.Join(src, ".beads"))); err != nil {
		t.Fatal(err)
	}

	if isGitTracked(filepath.Join(evil, ".beads", "config.yaml")) {
		t.Error("isGitTracked = true: a fake nested gitdir's index was trusted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("core.fsmonitor from a nested gitdir ran")
	}
}

func TestGitEnvWithoutRepoOverrides(t *testing.T) {
	got := gitEnvWithoutRepoOverrides([]string{"GIT_DIR=/x", "GIT_WORK_TREE=/y", "GIT_INDEX_FILE=/z", "PATH=/bin", "GIT_AUTHOR_NAME=a"})
	want := []string{"PATH=/bin", "GIT_AUTHOR_NAME=a"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
