//go:build windows

package main

import (
	"errors"
	"net"
)

// listenRestrictedUnix is refused on Windows. The unix-listener mode's security
// rests on a umask-tightened bind plus filesystem permissions (§3.3), and
// Windows has neither syscall.Umask nor the same permission semantics — so
// rather than bind something weaker under the same flag, this platform requires
// the mTLS TCP listener.
func listenRestrictedUnix(string) (net.Listener, error) {
	return nil, errors.New("unix listeners are not supported on windows; use a TCP listener with mutual TLS (§3.3)")
}
