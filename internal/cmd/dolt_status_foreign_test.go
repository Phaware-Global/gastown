package cmd

import (
	"net"
	"os"
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
	townRoot := t.TempDir()
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
	t.Chdir(townRoot)

	cfg := doltserver.DefaultConfig(townRoot)
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := doltserver.SaveState(townRoot, &doltserver.State{
		Running: true, PID: os.Getpid() + 1, Port: port, StartedAt: time.Now(), DataDir: cfg.DataDir,
	}); err != nil {
		t.Fatal(err)
	}

	err = runDoltStatus(doltStatusCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not serving") {
		t.Fatalf("runDoltStatus on foreign listener = %v, want unhealthy error", err)
	}
}
