package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

type noPRProbe struct{}

func (noPRProbe) FindPRNumber(string) (int, error) { return 0, nil }
func (noPRProbe) IsPRApproved(int) (bool, error)   { return false, nil }
func (noPRProbe) CountApprovals(int) (int, error)  { return 0, nil }

func hookTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newHookedCheckout builds a clone whose core.hooksPath is a tracked directory,
// with a pull request branch whose hooks append their name to marker.
func newHookedCheckout(t *testing.T) (work, marker string) {
	t.Helper()
	tmp := t.TempDir()
	marker = filepath.Join(tmp, "hooks-ran")
	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	work = filepath.Join(tmp, "work")

	commit := func(dir, name, content string, mode os.FileMode) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		hookTestGit(t, dir, "add", name)
		hookTestGit(t, dir, "commit", "-m", name)
	}
	identify := func(dir string) {
		hookTestGit(t, dir, "config", "user.email", "t@example.com")
		hookTestGit(t, dir, "config", "user.name", "T")
	}

	hookTestGit(t, tmp, "init", "--bare", "-b", "main", origin)
	hookTestGit(t, tmp, "clone", origin, seed)
	identify(seed)
	commit(seed, "base.txt", "base\n", 0o644)
	hookTestGit(t, seed, "push", "origin", "HEAD:main")

	hookTestGit(t, seed, "checkout", "-b", "feature")
	for _, hook := range []string{"post-checkout", "post-rewrite", "post-merge", "pre-push", "reference-transaction"} {
		commit(seed, ".githooks/"+hook, "#!/bin/sh\necho "+hook+" >> '"+marker+"'\n", 0o755)
	}
	hookTestGit(t, seed, "push", "origin", "feature", "feature:refs/pull/1/head")
	hookTestGit(t, seed, "checkout", "main")
	commit(seed, "main.txt", "main\n", 0o644)
	hookTestGit(t, seed, "push", "origin", "main")

	hookTestGit(t, tmp, "clone", origin, work)
	identify(work)
	// The PR's hooks are on disk before the operation under test starts, as in
	// a worktree left on a PR branch by an earlier review.
	hookTestGit(t, work, "checkout", "feature")
	hookTestGit(t, work, "config", "core.hooksPath", ".githooks")
	return work, marker
}

// clearGitConfigEnv gives the test an environment without inherited
// command-scope git config, restored afterwards.
func clearGitConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		t.Setenv(k, "")
	}
	t.Setenv("GIT_CONFIG_COUNT", "0")
}

func assertNoHookRan(t *testing.T, marker string) {
	t.Helper()
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("PR-authored hooks ran:\n%s", ran)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPRCheckoutGitRunsNoHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts need a POSIX shell")
	}

	// Git the agent types in its own session.
	sessionOps := map[string][][]string{
		"checkout": {{"checkout", "--detach", "origin/feature"}},
		"rebase":   {{"rebase", "origin/main"}},
		"merge":    {{"merge", "--no-edit", "origin/main"}},
		"update":   {{"fetch", "origin"}, {"pull", "--no-rebase", "--no-edit", "origin", "main"}},
		"push":     {{"push", "origin", "HEAD:refs/heads/copy"}},
		"worktree": {{"worktree", "add", "--detach", "../extra", "origin/feature"}},
	}
	for _, role := range []string{constants.RoleReviewer, constants.RoleRefinery} {
		for name, steps := range sessionOps {
			t.Run(role+" session "+name, func(t *testing.T) {
				clearGitConfigEnv(t)
				work, marker := newHookedCheckout(t)
				_ = os.Remove(marker)
				for k, v := range config.AgentEnv(config.AgentEnvConfig{Role: role, Rig: "rig"}) {
					if strings.HasPrefix(k, "GIT_CONFIG_") {
						t.Setenv(k, v)
					}
				}
				for _, args := range steps {
					hookTestGit(t, work, args...)
				}
				assertNoHookRan(t, marker)
			})
		}
	}

	// Git that gt itself runs, whatever environment it was started from.
	commandOps := []struct {
		name string
		cmd  *cobra.Command
		run  func(g *git.Git) error
	}{
		{"reviewer checkout", reviewerCheckoutCmd, func(g *git.Git) error {
			return g.CheckoutPRHeadDetached(1, "")
		}},
		{"refinery sync by rebase", refineryPrSyncBranchCmd, func(g *git.Git) error {
			_, err := refinery.SyncBranch(g, noPRProbe{}, "rebase", "feature", "main", "temp")
			return err
		}},
		{"refinery sync by merge and push", refineryPrPushBranchCmd, func(g *git.Git) error {
			if _, err := refinery.SyncBranch(g, noPRProbe{}, "merge", "feature", "main", "temp"); err != nil {
				return err
			}
			_, err := refinery.PushBranch(g, noPRProbe{}, "merge", "feature", "temp")
			return err
		}},
		{"integration land merge", mqIntegrationLandCmd, func(g *git.Git) error {
			if err := g.Checkout("main"); err != nil {
				return err
			}
			if err := g.Checkout("feature"); err != nil {
				return err
			}
			return g.MergeNoFF("origin/main", "land")
		}},
	}
	for _, op := range commandOps {
		t.Run("gt "+op.name, func(t *testing.T) {
			clearGitConfigEnv(t)
			work, marker := newHookedCheckout(t)
			_ = os.Remove(marker)
			if err := disableHooksForPRCheckoutCommands(op.cmd); err != nil {
				t.Fatal(err)
			}
			if err := op.run(git.NewGit(work)); err != nil {
				t.Fatal(err)
			}
			assertNoHookRan(t, marker)
		})
	}
}

// Polecats and other commands keep their hooks, and the shared repo config is
// left as it was.
func TestHooksStayEnabledOutsidePRCheckouts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts need a POSIX shell")
	}
	clearGitConfigEnv(t)
	work, marker := newHookedCheckout(t)
	_ = os.Remove(marker)

	env := config.AgentEnv(config.AgentEnvConfig{Role: constants.RolePolecat, Rig: "rig", AgentName: "p"})
	if v, ok := env["GIT_CONFIG_COUNT"]; ok {
		t.Errorf("polecat env sets GIT_CONFIG_COUNT=%s", v)
	}
	for _, cmd := range []*cobra.Command{mqSubmitCmd, refineryStartCmd, reviewerRequestCmd} {
		if err := disableHooksForPRCheckoutCommands(cmd); err != nil {
			t.Fatal(err)
		}
	}
	hookTestGit(t, work, "checkout", "--detach", "origin/feature")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("hooks did not run outside a PR checkout command: %v", err)
	}
	if got := hookTestGit(t, work, "config", "--local", "core.hooksPath"); got != ".githooks" {
		t.Errorf("repo core.hooksPath = %q, want .githooks", got)
	}
}
