package witness

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/tmux"
)

func finishedBase() finishedNotSubmittedInput {
	return finishedNotSubmittedInput{
		HeartbeatAge:    30 * time.Minute,
		HasHeartbeat:    true,
		HookOpen:        true,
		OnPolecatBranch: true,
		Clean:           true,
		AheadOfBase:     2,
	}
}

func TestIsFinishedNotSubmitted(t *testing.T) {
	const threshold = 15 * time.Minute
	if !isFinishedNotSubmitted(finishedBase(), threshold) {
		t.Fatal("pushed, clean, quiet, bead open: want nudge")
	}
	blockers := map[string]func(*finishedNotSubmittedInput){
		"no heartbeat":           func(i *finishedNotSubmittedInput) { i.HasHeartbeat = false },
		"recent heartbeat":       func(i *finishedNotSubmittedInput) { i.HeartbeatAge = time.Minute },
		"bead closed":            func(i *finishedNotSubmittedInput) { i.HookOpen = false },
		"already submitted":      func(i *finishedNotSubmittedInput) { i.Submitted = true },
		"no_merge or review-fix": func(i *finishedNotSubmittedInput) { i.NoMergeOrFix = true },
		"detached or base":       func(i *finishedNotSubmittedInput) { i.OnPolecatBranch = false },
		"dirty or unpushed":      func(i *finishedNotSubmittedInput) { i.Clean = false },
		"nothing ahead of base":  func(i *finishedNotSubmittedInput) { i.AheadOfBase = 0 },
	}
	for name, mutate := range blockers {
		t.Run(name, func(t *testing.T) {
			in := finishedBase()
			mutate(&in)
			if isFinishedNotSubmitted(in, threshold) {
				t.Fatalf("%s: want no nudge", name)
			}
		})
	}
}

// finishedWorktree builds <town>/<rig>/polecats/<polecat>/<rig> as a clone of a
// bare origin, checked out on branch with one pushed commit ahead of origin/HEAD.
func finishedWorktree(t *testing.T, townRoot, rig, polecatName, branch string) {
	t.Helper()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	scratch := t.TempDir()
	origin := filepath.Join(scratch, "origin.git")
	seed := filepath.Join(scratch, "seed")
	run(scratch, "init", "--bare", "-b", "main", origin)
	run(scratch, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", ".")
	run(seed, "commit", "-m", "base")
	run(seed, "push", origin, "main")

	clone := filepath.Join(townRoot, rig, "polecats", polecatName, rig)
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	run(scratch, "clone", origin, clone)
	run(clone, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(clone, "add", ".")
	run(clone, "commit", "-m", "work")
	run(clone, "push", "-u", "origin", branch)
}

// TestDetectFinishedNotSubmitted_BranchOwnership drives the real detector against
// a real worktree: the nudge tells a worker to run gt done, so it must only fire
// when the checked-out branch is this polecat's own branch for its hooked bead.
// The session name must not match a live tmux session: a delivered nudge would
// land in a real agent.
func TestDetectFinishedNotSubmitted_BranchOwnership(t *testing.T) {
	const (
		rig         = "gastown"
		polecatName = "nightrider"
		hook        = "gt-abcd"
	)
	cases := []struct {
		name   string
		branch string
		want   bool
	}{
		{"own branch for hooked bead", "polecat/nightrider/gt-abcd@mv5w1j3s", true},
		{"own branch, bead id without suffix", "polecat/nightrider/gt-abcd", true},
		{"another polecat's branch", "polecat/capable/gt-abcd@mv5w1j3s", false},
		{"another polecat's branch for another bead", "polecat/capable/gt-4kip@mv5w1j3s", false},
		{"own name, different bead", "polecat/nightrider/gt-zzzz@mv5w1j3s", false},
		{"hooked bead id only as a prefix of the branch's bead", "polecat/nightrider/gt-abcdef@mv5w1j3s", false},
		{"legacy name-timestamp branch", "polecat/nightrider-mv5w1j3s", false},
		{"not a polecat branch", "feature/gt-abcd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			finishedWorktree(t, townRoot, rig, polecatName, tc.branch)

			bd := &BdCli{
				Exec: func(workDir string, args ...string) (string, error) {
					return `[{"id":"gt-abcd","status":"hooked","description":""}]`, nil
				},
				Run: func(string, ...string) error { return nil },
			}
			hb := &polecat.SessionHeartbeat{Timestamp: time.Now().Add(-time.Hour)}
			snap := &agentBeadSnapshot{HookBead: hook}

			_, got := detectFinishedNotSubmitted(bd, townRoot, townRoot, rig, polecatName, "gt-test-nonexistent-session", tmux.NewTmux(), hb, snap, 15*time.Minute)
			if got != tc.want {
				t.Fatalf("branch %q: nudged=%v, want %v", tc.branch, got, tc.want)
			}
		})
	}
}
