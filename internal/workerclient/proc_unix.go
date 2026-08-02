//go:build !windows

package workerclient

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the agent in its own process group so a signal reaches
// its whole tree, not just the binary gastown exec'd.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup sends sig to the agent's entire group. The negative pid is
// what makes it a group signal (see setProcessGroup).
func signalProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
