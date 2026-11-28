package git

import (
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/constants"
)

// SetHooksPathForProcess makes every git run by this process and its children,
// including git started by gh or a shell, take its hooks from dir.
func SetHooksPathForProcess(dir string) error {
	for k, v := range constants.GitHooksPathEnv(dir, os.Getenv) {
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("setting git hooks path: %w", err)
		}
	}
	return nil
}
