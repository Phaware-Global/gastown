//go:build darwin

package daemon

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// BSD process run-states (sys/proc.h), stable across macOS versions.
const darwinSZOMB = 5 // exited, awaiting reap by parent

// isProcessAlive checks if a process is still running.
//
// signal(pid, 0) alone is not enough on darwin: the kernel keeps a PID slot
// (and answers signal probes for it) until the process is reaped by its
// parent, so a zombie still reports "alive". For a process this daemon did
// not fork itself — e.g. an adopted subprocess recovered from a PID file —
// nothing here ever calls wait() on it, so if the real parent (typically
// launchd, after reparenting) is slow to reap it, a plain signal-0 check can
// report the process as running indefinitely after it has actually exited.
// Query the kernel's real process state via sysctl and treat a zombie as not
// alive, so callers see the exit promptly instead of never.
func isProcessAlive(p *os.Process) bool {
	if p.Signal(syscall.Signal(0)) != nil {
		return false
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", p.Pid)
	if err != nil {
		// Couldn't query kernel state (e.g. sysctl unavailable); fall back to
		// the signal-based result already obtained above.
		return true
	}
	return info.Proc.P_stat != darwinSZOMB
}
