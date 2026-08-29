//go:build unix && !darwin

package daemon

import (
	"os"
	"syscall"
)

// isProcessAlive checks if a process is still running.
func isProcessAlive(p *os.Process) bool {
	return p.Signal(syscall.Signal(0)) == nil
}
