package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// isGitTracked reports whether path is a file tracked by the git repository
// that contains it. A path outside any repository, or any git failure, is
// reported as untracked: there is no worktree to dirty.
//
// The doctor walk reaches polecat worktrees and reviewer PR checkouts, whose
// contents are attacker-controlled. A branch can commit a fake gitdir
// (HEAD, config, index, objects/, refs/) beside a .beads dir; git would adopt
// it as the repository and run its core.fsmonitor command. Protected -c
// settings refuse bare/nested gitdir discovery and disable fsmonitor, and the
// GIT_* repository overrides are dropped so the caller's env cannot redirect
// the lookup either.
func isGitTracked(path string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	cmd := exec.Command("git",
		"-c", "safe.bareRepository=explicit",
		"-c", "core.fsmonitor=false",
		"-C", filepath.Dir(path),
		"ls-files", "--error-unmatch", "--", filepath.Base(path))
	cmd.Env = gitEnvWithoutRepoOverrides(os.Environ())
	return cmd.Run() == nil
}

// gitEnvWithoutRepoOverrides returns env minus the variables that point git at
// a specific gitdir, worktree or index.
func gitEnvWithoutRepoOverrides(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE":
			continue
		}
		out = append(out, kv)
	}
	return out
}
