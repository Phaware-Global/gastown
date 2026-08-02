//go:build windows

package workerclient

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on windows: there is no Setpgid, and the worker's
// signal model (graceful SIGTERM to the agent's whole tree, then a hard kill on
// WaitDelay) has no equivalent here. The worker client targets unix hosts; this
// file exists so the package still builds and is smoke-tested cross-platform.
func setProcessGroup(*exec.Cmd) {}

// signalProcessGroup is unsupported on windows. Callers treat the error as
// "the signal could not be delivered" rather than assuming it landed.
func signalProcessGroup(int, syscall.Signal) error {
	return errors.New("signalling a process group is not supported on windows")
}
