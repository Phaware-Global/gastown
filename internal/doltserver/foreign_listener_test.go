package doltserver

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// listenForeign binds a free loopback port from this test process, standing in
// for a dolt sql-server auto-started by bd from a .beads/dolt dir. The test
// binary has no --data-dir/--config flag and its cwd is the package dir, so it
// is not the town's server.
func listenForeign(t *testing.T) int {
	t.Helper()
	t.Cleanup(SetDoltProcessDetectorForTest(func(int) bool { return true }))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
	return ln.Addr().(*net.TCPAddr).Port
}

func TestVerifyServerDataDir_ForeignListenerWithStaleState(t *testing.T) {
	townRoot := t.TempDir()
	port := listenForeign(t)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")

	config := DefaultConfig(townRoot)
	if err := os.MkdirAll(config.DataDir, 0755); err != nil {
		t.Fatal(err)
	}
	// State left behind by the canonical server that went down: right data dir,
	// a PID that is not the listener.
	if err := SaveState(townRoot, &State{
		Running:   true,
		PID:       os.Getpid() + 1,
		Port:      port,
		StartedAt: time.Now(),
		DataDir:   config.DataDir,
	}); err != nil {
		t.Fatal(err)
	}

	legitimate, err := VerifyServerDataDir(townRoot)
	if legitimate {
		t.Fatalf("foreign listener on port %d reported legitimate (err=%v)", port, err)
	}
	if err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
		t.Fatalf("want ownership mismatch error, got %v", err)
	}
}

func TestForeignListener(t *testing.T) {
	townRoot := t.TempDir()
	port := listenForeign(t)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")

	pid, owner, foreign := ForeignListener(townRoot)
	if !foreign || pid != os.Getpid() {
		t.Fatalf("ForeignListener = (%d, %q, %v), want pid %d foreign", pid, owner, foreign, os.Getpid())
	}
	cwd, _ := os.Getwd()
	if filepath.Clean(owner) != filepath.Clean(cwd) {
		t.Errorf("owner = %q, want cwd %q", owner, cwd)
	}
}

func TestForeignListener_NothingListening(t *testing.T) {
	townRoot := t.TempDir()
	port := listenForeign(t)
	// Close by picking a different, unbound port.
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port+1))
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")

	if pid, _, foreign := ForeignListener(townRoot); foreign {
		t.Fatalf("no listener on port, got foreign pid %d", pid)
	}
}

// A port forwarded into a container is held by a non-dolt process (docker-proxy,
// vpnkit, ssh -L). It must not read as a squatter: that would mark a healthy
// containerized server UNHEALTHY and have the daemon kill the forwarder.
func TestForeignListener_NonDoltListenerIsNotForeign(t *testing.T) {
	townRoot := t.TempDir()
	port := listenForeign(t)
	t.Cleanup(SetDoltProcessDetectorForTest(func(int) bool { return false }))
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")

	if pid, _, foreign := ForeignListener(townRoot); foreign {
		t.Fatalf("non-dolt listener pid %d reported foreign", pid)
	}
}
