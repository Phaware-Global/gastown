package constants

// GitHooksDisabledEnv returns the environment under which git runs no
// client-side hook: command-scope config that overrides any core.hooksPath in
// repo or worktree config without writing to it.
//
// It is for processes that run git against content they did not author. A
// repository's hooks path may be a tracked directory, in which case checking
// out a branch also chooses the hooks that run next.
func GitHooksDisabledEnv() map[string]string {
	return map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "core.hooksPath",
		"GIT_CONFIG_VALUE_0": "/dev/null",
	}
}
