//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// checkPrivateDir requires dir to be a directory this user owns and nobody
// else can write to. A shared /tmp fails both.
func checkPrivateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is not a private directory of this user", dir)
	}
	return nil
}

// lsofPath finds lsof, including where macOS keeps it, which a service
// manager's PATH may leave out.
func lsofPath() (string, error) {
	if p, err := exec.LookPath("lsof"); err == nil {
		return p, nil
	}
	return exec.LookPath("/usr/sbin/lsof")
}

func listCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, goTmpListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: args are constructed internally
	cmd.WaitDelay = goTmpListTimeout / 12
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s: %w", name, ctx.Err()) // partial output
	}
	return string(out), err
}

// goTmpHolders lists the files this user's processes have open (including
// working directories and running binaries) and their command lines.
func goTmpHolders(ctx context.Context) (string, error) {
	lsof, err := lsofPath()
	if err != nil {
		return "", err
	}
	// lsof exits non-zero over unrelated warnings, so completeness is judged
	// by whether the listing includes this process.
	open, err := listCommand(ctx, lsof, "-n", "-P", "-w", "-u", strconv.Itoa(os.Getuid()), "-F", "pn")
	if !strings.Contains("\n"+open, "\np"+strconv.Itoa(os.Getpid())+"\n") {
		return "", fmt.Errorf("lsof listing is incomplete: %v", err)
	}
	commands, err := listCommand(ctx, "ps", "-axww", "-o", "command=")
	if err != nil {
		return "", fmt.Errorf("ps: %w", err)
	}
	return open + "\n" + commands, nil
}
