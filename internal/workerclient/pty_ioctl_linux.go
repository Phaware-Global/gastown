//go:build linux

package workerclient

import "golang.org/x/sys/unix"

// See pty_ioctl_bsd.go.
const ioctlGet = unix.TCGETS
