//go:build !windows

package githooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func checkOwnership(info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has no owner information")
	}
	return checkOwnerAndMode(int(st.Uid), os.Geteuid(), info.Mode())
}

func checkOwnerAndMode(owner, user int, mode fs.FileMode) error {
	if owner != user {
		return fmt.Errorf("is owned by uid %d, not %d", owner, user)
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("is group or world writable (%s)", mode.Perm())
	}
	return nil
}
