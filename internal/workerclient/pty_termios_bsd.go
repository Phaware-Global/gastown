//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package workerclient

import "golang.org/x/sys/unix"

// Termios ioctl numbers are platform-specific: the BSDs (macOS included) use
// TIOCGETA/TIOCSETA where Linux uses TCGETS/TCSETS. Naming them in build-tagged
// files is what keeps the worker cross-compilable — `make dist` builds this
// binary for linux/{amd64,arm64} from a Mac, so a darwin-only constant here is a
// build break for the platform most workers actually run.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
