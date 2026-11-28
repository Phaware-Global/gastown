//go:build !windows

package tmuxguard

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func checkOwnerOnly(dir string, info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by this user", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("%s has mode %#o, want 0700", dir, perm)
	}
	return nil
}
