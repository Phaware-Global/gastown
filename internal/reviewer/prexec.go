package reviewer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Code from a pull request (its tests, its build scripts) runs through
// RunPRCommand, not in the reviewer's own environment. That environment leads to
// the town's tmux server and Dolt server, and a branch's tests may act on
// whatever they can reach. This guards against accidents, not against hostile
// code: it can still open the town's socket or Dolt port by name.

// townReachVars are removed from the environment of a PR command.
var townReachVars = []string{
	"TMUX", "TMUX_PANE", "TMUX_TMPDIR", "GT_TOWN_SOCKET", "GT_TMUX_SOCKET",
	"GT_TOWN_ROOT", "GT_ROOT",
	"GT_DOLT_PORT", "BEADS_DOLT_PORT", "BEADS_DOLT_SERVER_PORT",
	"GT_DOLT_HOST", "BEADS_DOLT_HOST", "BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_AUTO_START",
}

// deadDoltPort replaces the Dolt port. Unsetting it is not enough: without a
// port, gt and bd fall back to the production server's.
const deadDoltPort = "1"

// prCommandEnv returns environ without townReachVars, with tmux pointed at
// tmuxDir and Dolt at a port nothing listens on.
func prCommandEnv(environ []string, tmuxDir string) []string {
	env := slices.DeleteFunc(slices.Clone(environ), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(townReachVars, name)
	})
	return append(env,
		"TMUX_TMPDIR="+tmuxDir,
		"GT_DOLT_PORT="+deadDoltPort,
		"BEADS_DOLT_PORT="+deadDoltPort,
		// bd would otherwise start a server of its own on finding none.
		"BEADS_DOLT_AUTO_START=0",
	)
}

// RunPRCommand runs argv with the environment of prCommandEnv and a private
// TMUX_TMPDIR that is removed afterwards, along with any tmux server started
// in it. It returns the command's exit code; err reports a command that did not
// run.
func RunPRCommand(argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	base := "/tmp" // socket paths are length-limited, and os.TempDir is long on macOS
	if runtime.GOOS == "windows" {
		base = ""
	}
	tmuxDir, err := os.MkdirTemp(base, "gt-pr-")
	if err != nil {
		return 0, fmt.Errorf("making a private TMUX_TMPDIR: %w", err)
	}
	defer removeTmuxDir(tmuxDir)

	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the caller's command
	c.Env = prCommandEnv(os.Environ(), tmuxDir)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	// A background process the command leaves behind must not hold us open.
	c.WaitDelay = 10 * time.Second

	// Signals are passed on, so that the directory is still removed.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	if err := c.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case sig := <-signals:
			_ = c.Process.Signal(sig)
		case err := <-done:
			var exit *exec.ExitError
			switch {
			case errors.As(err, &exit):
				if code := exit.ExitCode(); code >= 0 {
					return code, nil
				}
				return 1, nil // killed by a signal
			case errors.Is(err, exec.ErrWaitDelay):
				return c.ProcessState.ExitCode(), nil
			}
			return 0, err
		}
	}
}

// removeTmuxDir stops the tmux servers whose sockets are under dir, then
// removes it.
func removeTmuxDir(dir string) {
	sockets, _ := filepath.Glob(filepath.Join(dir, "tmux-*", "*"))
	for _, socket := range sockets {
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run() //nolint:gosec // a socket under our own directory
	}
	_ = os.RemoveAll(dir)
}
