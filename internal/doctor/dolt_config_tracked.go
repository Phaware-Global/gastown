package doctor

import (
	"os/exec"
	"path/filepath"
)

// isGitTracked reports whether path is a file tracked by the git repository
// that contains it. A path outside any repository, or any git failure, is
// reported as untracked: there is no worktree to dirty.
func isGitTracked(path string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	cmd := exec.Command("git", "-C", filepath.Dir(path), "ls-files", "--error-unmatch", "--", filepath.Base(path))
	return cmd.Run() == nil
}
