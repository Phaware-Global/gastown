//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// listenRestrictedUnix binds a unix socket that is never, even briefly, exposed
// at default permissions (§3.3 fs gate): the umask is tightened around the bind
// itself, closing the window between Listen and Chmod, and restored immediately.
//
// 0600, not 0700: the execute bit is meaningless on a socket, and connecting
// needs only write permission.
func listenRestrictedUnix(path string) (net.Listener, error) {
	_ = os.Remove(path) // stale socket from a previous run
	oldMask := syscall.Umask(0o077)
	ln, err := net.Listen("unix", path)
	syscall.Umask(oldMask)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}
	return ln, nil
}
