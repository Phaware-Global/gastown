//go:build !windows

package nudge

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func pollerProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return proc.Signal(syscall.Signal(0)) == nil
}

// pollerProcessIsOurs reports whether the process is a nudge-poller for the
// session: its command line names the "nudge-poller" subcommand and the
// session. A PID file can outlive its poller, and the PID can be reused by an
// unrelated process that must never be signaled.
func pollerProcessIsOurs(pid int, session string) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "nudge-poller" && i+1 < len(fields) && fields[i+1] == session {
			return true
		}
	}
	return false
}
