//go:build linux

package workerclient

import "golang.org/x/sys/unix"

// See pty_ioctl_bsd.go for why these are build-tagged.
const (
	ioctlGet = unix.TCGETS
	ioctlSet = unix.TCSETS
)
