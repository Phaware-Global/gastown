//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyWindowChange delivers terminal-resize notifications on ch. SIGWINCH is
// how a terminal reports that its geometry changed, and forwarding it is what
// keeps the remote pty the same size as the local pane.
func notifyWindowChange(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}
