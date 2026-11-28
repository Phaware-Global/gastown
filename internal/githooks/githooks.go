// Package githooks provides the hooks directory used by sessions that run git
// over pull request content. gt writes it under the town root, outside every
// checkout, so a checked-out branch can neither choose nor change the hooks
// that run.
package githooks

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/constants"
)

//go:embed pre-push
var prePush []byte

const prePushName = "pre-push"

// blockedConfigCount is not a number, which makes every git command fail.
const blockedConfigCount = "gt-trusted-hooks-unavailable"

// Dir returns the trusted hooks directory of a town.
func Dir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "trusted-hooks")
}

// Install writes the guards into the town's trusted hooks directory and returns
// it. An existing directory that fails Verify's checks is refused, not repaired.
func Install(townRoot string) (string, error) {
	if !filepath.IsAbs(townRoot) {
		return "", fmt.Errorf("trusted hooks: town root %q is not an absolute path", townRoot)
	}
	if _, err := os.Stat(townRoot); err != nil {
		return "", fmt.Errorf("trusted hooks: town root: %w", err)
	}
	dir := Dir(townRoot)
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("trusted hooks: %w", err)
	}
	switch err := os.Mkdir(dir, 0o700); {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		if err := verifyPath(dir, true); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("trusted hooks: %w", err)
	}
	if err := writeHook(parent, filepath.Join(dir, prePushName), prePush); err != nil {
		return "", fmt.Errorf("trusted hooks: %w", err)
	}
	if err := Verify(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// writeHook replaces path atomically, so a concurrent git never runs a partial
// hook. The temporary file stays out of the hooks directory.
func writeHook(tmpDir, path string, content []byte) error {
	tmp, err := os.CreateTemp(tmpDir, ".hook-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(content)
	if err := errors.Join(err, tmp.Chmod(0o700), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Verify reports whether dir is safe to hand to git as core.hooksPath: the
// directory and its guard exist, are not symlinks, belong to the current user
// and are not group or world writable.
func Verify(dir string) error {
	if err := verifyPath(dir, true); err != nil {
		return err
	}
	return verifyPath(filepath.Join(dir, prePushName), false)
}

func verifyPath(path string, wantDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("trusted hooks: %w", err)
	}
	if wantDir && !info.IsDir() || !wantDir && !info.Mode().IsRegular() {
		return fmt.Errorf("trusted hooks: %s has the wrong file type (%s)", path, info.Mode())
	}
	if err := checkOwnership(info); err != nil {
		return fmt.Errorf("trusted hooks: %s %w", path, err)
	}
	return nil
}

// RefineryEnv installs the guards and returns the environment under which git
// runs them in place of any hooks the repository configures.
func RefineryEnv(townRoot string) (map[string]string, error) {
	dir, err := Install(townRoot)
	if err != nil {
		return nil, err
	}
	return constants.GitHooksPathEnv(dir, os.Getenv), nil
}

// RefinerySessionEnv is RefineryEnv for callers that cannot return an error. If
// the guards cannot be trusted, the environment makes every git command fail
// rather than run with the repository's hooks or with none.
func RefinerySessionEnv(townRoot string) map[string]string {
	env, err := RefineryEnv(townRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: git is disabled for the refinery: %v\n", err)
		return map[string]string{"GIT_CONFIG_COUNT": blockedConfigCount}
	}
	return env
}
