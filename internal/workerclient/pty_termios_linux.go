//go:build linux

package workerclient

import "golang.org/x/sys/unix"

// See pty_termios_bsd.go for why these are build-tagged.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
