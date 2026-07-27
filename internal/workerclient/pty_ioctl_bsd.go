//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package workerclient

import "golang.org/x/sys/unix"

// ioctlGet is the termios read ioctl, used by tests to inspect what startPTY did
// to the line discipline. Build-tagged because the BSDs (macOS included) use
// TIOCGETA where Linux uses TCGETS — a darwin-only constant here previously
// broke the Linux build of the whole worker.
const ioctlGet = unix.TIOCGETA
