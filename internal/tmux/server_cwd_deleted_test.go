//go:build !windows

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServerInDeletedDir starts a dedicated tmux server from a directory that
// is then removed, reproducing gt-a7pkv: the server keeps the dead directory as
// its working directory and hands it to every new pane, ignoring -c.
func startServerInDeletedDir(t *testing.T) *Tmux {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := fmt.Sprintf("gt-test-deadcwd-%d", os.Getpid())
	doomed := filepath.Join(t.TempDir(), "doomed")
	if err := os.Mkdir(doomed, 0755); err != nil {
		t.Fatal(err)
	}
	start := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "base", "sleep 120")
	start.Dir = doomed
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	if err := os.Remove(doomed); err != nil {
		t.Fatal(err)
	}
	return NewTmuxWithSocket(socket)
}

// waitForPwd returns the directory the pane command recorded in outFile.
func waitForPwd(t *testing.T, outFile string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(outFile); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane command never wrote %s", outFile)
	return ""
}

func TestPaneStartsInWorkDirWhenServerCwdDeleted(t *testing.T) {
	workDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pwdTo := func(name string) (string, string) {
		out := filepath.Join(workDir, name)
		return out, fmt.Sprintf("sh -c '/bin/pwd > %s 2>&1; sleep 5'", out)
	}

	t.Run("NewSessionWithCommand", func(t *testing.T) {
		tm := startServerInDeletedDir(t)
		out, cmd := pwdTo("new.out")
		if err := tm.NewSessionWithCommand("gt-test-deadcwd-new", workDir, cmd); err != nil {
			t.Fatalf("NewSessionWithCommand: %v", err)
		}
		if got := waitForPwd(t, out); got != workDir {
			t.Errorf("pane cwd = %q, want %q", got, workDir)
		}
	})

	t.Run("NewSessionWithCommandAndEnv", func(t *testing.T) {
		tm := startServerInDeletedDir(t)
		out, cmd := pwdTo("env.out")
		if err := tm.NewSessionWithCommandAndEnv("gt-test-deadcwd-env", workDir, cmd, map[string]string{"GT_TEST": "1"}); err != nil {
			t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
		}
		if got := waitForPwd(t, out); got != workDir {
			t.Errorf("pane cwd = %q, want %q", got, workDir)
		}
	})

	t.Run("RespawnPaneWithWorkDir", func(t *testing.T) {
		tm := startServerInDeletedDir(t)
		out, cmd := pwdTo("respawn.out")
		if err := tm.RespawnPaneWithWorkDir("base", workDir, cmd); err != nil {
			t.Fatalf("RespawnPaneWithWorkDir: %v", err)
		}
		if got := waitForPwd(t, out); got != workDir {
			t.Errorf("pane cwd = %q, want %q", got, workDir)
		}
	})
}
