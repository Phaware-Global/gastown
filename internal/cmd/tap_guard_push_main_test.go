package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIsPushToMain covers the push-refspec classifier for the push-main
// guard. The incident that motivated the guard used
// `git push origin FETCH_HEAD:refs/heads/main`, which doesn't look like
// "push to main" in a naïve check — the destination is buried after a
// colon in the refspec. We explicitly test that shape plus common
// variants, and keep negative cases that must pass through.
//
// Bare "git push" / "git push origin" (no refspec) defer to the current
// branch and aren't in this table — see TestBarePushDefersToCurrentBranch
// for that path in isolation, so the outcome isn't coupled to the
// gastown repo's own HEAD state during CI.
func TestIsPushToMain(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		// Positives — must be blocked.
		{"bare push to main", "git push origin main", true},
		{"HEAD to main", "git push origin HEAD:main", true},
		{"HEAD to refs/heads/main", "git push origin HEAD:refs/heads/main", true},
		{"FETCH_HEAD to refs/heads/main (observed)", "git push origin FETCH_HEAD:refs/heads/main", true},
		{"branch to main", "git push origin polecat/foo:main", true},
		{"branch to refs/heads/main", "git push origin polecat/foo:refs/heads/main", true},
		{"with -f flag", "git push -f origin main", true},
		{"with --force-with-lease", "git push --force-with-lease origin main", true},
		{"leading whitespace", "   git push origin main", true},

		// New positives — augment-review bypass cases.
		{"multi-refspec, main among them", "git push origin main HEAD:other", true},
		{"main among many refspecs", "git push origin feat/a feat/b main feat/c", true},
		{"--all (implicitly includes main)", "git push origin --all", true},
		{"--mirror (mirrors all refs)", "git push origin --mirror", true},
		{"--all with force", "git push --force origin --all", true},

		// Negatives — must pass through.
		{"push to feature branch", "git push origin polecat/foo", false},
		{"branch:branch", "git push origin polecat/foo:polecat/foo", false},
		{"branch to non-main refspec", "git push origin polecat/foo:feat/thing", false},
		{"fetch not push", "git fetch origin main", false},
		{"echo containing push", `echo "git push origin main"`, false},
		{"push to mainline (not main)", "git push origin mainline", false},
		{"push to main/foo (not main)", "git push origin HEAD:main/foo", false},
		{"not a git command", "ls", false},
		{"git without push", "git status", false},
		{"main as source, branch as dest", "git push origin main:polecat/foo", false},

		// New negatives — augment-review false-positive cases.
		{"-o main (flag value, not refspec)", "git push origin feature -o main", false},
		{"--push-option main (flag value)", "git push origin feature --push-option main", false},
		{"--receive-pack main (flag value)", "git push origin feature --receive-pack main", false},
		{"--push-option=main (= form already filtered)", "git push origin feature --push-option=main", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPushToMain(tc.cmd)
			if got != tc.want {
				t.Errorf("isPushToMain(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestBarePushDefersToCurrentBranch isolates the "no refspec provided"
// path. It builds a temp repo whose current branch can be controlled,
// so the result isn't coupled to whatever branch the gastown repo is on
// during test runs.
func TestBarePushDefersToCurrentBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, skipping current-branch tests")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch=feature", filepath.Clean(dir))
	run("config", "user.email", "t@test")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")

	// Run tests from inside the temp repo so currentBranchIsMain's
	// exec.Command("git", "rev-parse", …) resolves to this repo.
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}

	for _, cmd := range []string{
		"git push",
		"git push origin",
	} {
		if isPushToMain(cmd) {
			t.Errorf("isPushToMain(%q) = true while on branch=feature; want false", cmd)
		}
	}

	// Switch to main and re-test — now the same commands target main.
	run("checkout", "-b", "main")
	for _, cmd := range []string{
		"git push",
		"git push origin",
	} {
		if !isPushToMain(cmd) {
			t.Errorf("isPushToMain(%q) = false while on branch=main; want true", cmd)
		}
	}
}

// TestIdentifyCurrentRigEscapeGuard ensures GT_RIG with embedded path
// separators / `..` / `.` is rejected rather than trusted — otherwise a
// malicious or mistakenly-set GT_RIG could cause the settings lookup to
// read a config from an unintended location.
func TestIdentifyCurrentRigEscapeGuard(t *testing.T) {
	town := t.TempDir()
	cases := []struct {
		name, val, want string
	}{
		{"dotdot-escape", "../escaped", ""},
		{"slash-in-name", "foo/bar", ""},
		{"backslash-in-name", "foo\\bar", ""},
		{"dotdot-alone", "..", ""},
		{"dot-alone", ".", ""},
		{"well-formed", "gastown", "gastown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GT_RIG", tc.val)
			if got := identifyCurrentRig(town); got != tc.want {
				t.Errorf("GT_RIG=%q resolved to %q, want %q", tc.val, got, tc.want)
			}
		})
	}
}
