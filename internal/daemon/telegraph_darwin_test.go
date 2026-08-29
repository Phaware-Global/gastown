//go:build darwin

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// spawnZombie starts a real child process that exits almost immediately and
// is deliberately never reaped by the test (mirroring an "adopted" process
// this daemon did not fork itself), then blocks until the kernel actually
// reports it as a zombie. The caller must reap it via t.Cleanup to avoid
// leaking a zombie in the test runner's process tree.
func spawnZombie(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawning short-lived child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err == nil && info.Proc.P_stat == darwinSZOMB {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child PID %d never became a zombie", pid)
	return 0
}

// TestIsProcessAlive_ZombieIsNotAlive documents and pins down the darwin
// kernel behavior at the root of gt-23no: signal(pid, 0) alone reports a
// zombie as alive because the kernel still holds its PID slot until it is
// reaped. isProcessAlive must see through that via the real process state.
func TestIsProcessAlive_ZombieIsNotAlive(t *testing.T) {
	pid := spawnZombie(t)

	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("os.FindProcess: %v", err)
	}

	if process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("precondition failed: raw signal(pid, 0) should still succeed against a zombie")
	}

	if isProcessAlive(process) {
		t.Fatal("isProcessAlive must report a zombie process as not alive")
	}
}

// TestEnsureRunning_RestartsAdoptedZombie reproduces the gt-23no incident at
// the TelegraphServerManager level: the daemon adopts a pre-existing
// Telegraph via its PID file, the real health-check machinery (isRunning /
// isProcessAlive) is exercised unmocked, and once the adopted process exits
// the very next health tick must restart it rather than believing it is
// still running forever.
func TestEnsureRunning_RestartsAdoptedZombie(t *testing.T) {
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 5,
		RestartWindow:       time.Minute,
	}
	m := newTestTelegraphManager(t, cfg)

	pid := spawnZombie(t)
	if err := os.MkdirAll(filepath.Dir(m.pidFile()), 0755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if _, err := writePIDFile(m.pidFile(), pid); err != nil {
		t.Fatalf("writePIDFile: %v", err)
	}

	var starts int32
	m.startFn = func() error {
		atomic.AddInt32(&starts, 1)
		return nil
	}

	// Adoption happens through the real isRunning()/isProcessAlive() path —
	// runningFn is deliberately left unset.
	m.EnsureRunning()

	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("expected EnsureRunning to restart the adopted-but-dead Telegraph, got %d start attempts", got)
	}
}
