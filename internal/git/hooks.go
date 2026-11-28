package git

import (
	"fmt"
	"os"
	"strconv"

	"github.com/steveyegge/gastown/internal/constants"
)

// DisableHooksForProcess makes every git run by this process and its children
// skip client-side hooks, including git started by gh or a shell. Config the
// environment already carries is kept: the entry is appended after it.
func DisableHooksForProcess() error {
	n, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if err != nil || n < 0 {
		n = 0
	}
	env := constants.GitHooksDisabledEnv()
	for _, kv := range [][2]string{
		{fmt.Sprintf("GIT_CONFIG_KEY_%d", n), env["GIT_CONFIG_KEY_0"]},
		{fmt.Sprintf("GIT_CONFIG_VALUE_%d", n), env["GIT_CONFIG_VALUE_0"]},
		{"GIT_CONFIG_COUNT", strconv.Itoa(n + 1)},
	} {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return fmt.Errorf("disabling git hooks: %w", err)
		}
	}
	return nil
}
