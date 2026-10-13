package cmd

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// A listener on the Dolt port that isn't the town's server (a bd auto-started
// sql-server from a .beads/dolt dir) must not read as a healthy server, even
// when the town's state file still names the canonical data dir.
func TestDoltStatus_ForeignListenerIsUnhealthy(t *testing.T) {
	// A state-file PID that is not the listener, both dead (the server went
	// down) and alive (PID reuse, or any live process), must not vouch for it.
	for name, pid := range map[string]int{
		"dead state pid": deadPID(t),
		"live state pid": os.Getpid(),
		"other live pid": os.Getppid(),
	} {
		t.Run(name, func(t *testing.T) { assertForeignListenerUnhealthy(t, pid) })
	}
}

// deadPID returns a PID that is not running: a child that has been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Skipf("cannot spawn child: %v", err)
	}
	return c.Process.Pid
}

func assertForeignListenerUnhealthy(t *testing.T, statePID int) {
	t.Helper()
	// Resolve symlinks so the town path is spelled the same on every platform:
	// macOS /var -> /private/var otherwise hides path-equality bugs that Linux hits.
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")
	// Run from inside the town but not at its root: a process whose cwd is the
	// town root (or the data dir) is how the town's own server looks, so a test
	// listener there would legitimately read as healthy.
	t.Chdir(filepath.Join(townRoot, "mayor"))

	cfg := doltserver.DefaultConfig(townRoot)
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := doltserver.SaveState(townRoot, &doltserver.State{
		Running: true, PID: statePID, Port: port, StartedAt: time.Now(), DataDir: cfg.DataDir,
	}); err != nil {
		t.Fatal(err)
	}

	err = runDoltStatus(doltStatusCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not serving") {
		t.Fatalf("runDoltStatus on foreign listener = %v, want unhealthy error", err)
	}
}
