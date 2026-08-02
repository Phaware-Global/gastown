package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// writeFakeTmuxWithCreatedTime creates a fake tmux where has-session succeeds
// and list-sessions reports the given session_created Unix timestamp (used by
// GetSessionCreatedTime). display-message reports an active agent so the
// idle-based reap paths would never fire.
func writeFakeTmuxWithCreatedTime(t *testing.T, dir string, created time.Time) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\n"+
		"case \"$*\" in\n"+
		"  *has-session*) exit 0;;\n"+
		"  *list-sessions*) echo '%d';;\n"+
		"  *display-message*) echo 'claude';;\n"+
		"  *kill-session*) exit 0;;\n"+
		"  *) exit 1;;\n"+
		"esac\n", created.Unix())
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0755); err != nil {
		t.Fatalf("writing fake tmux: %v", err)
	}
}

func maxRuntimeTestDaemon(t *testing.T, created time.Time) (*Daemon, *strings.Builder) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux")
	}
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })

	binDir := t.TempDir()
	writeFakeTmuxWithCreatedTime(t, binDir, created)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmuxWithSocket(""),
	}

	// Fresh heartbeat in "working" state — only the max-runtime cap could reap this.
	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0755)
	hb := polecat.SessionHeartbeat{Timestamp: time.Now().UTC(), State: polecat.HeartbeatWorking}
	data, _ := json.Marshal(hb)
	_ = os.WriteFile(hbPath, data, 0644)

	return d, &logBuf
}

// TestReapIdlePolecat_MaxRuntimeExceeded verifies the absolute zombie cap:
// a busy polecat (fresh heartbeat, live agent) past max_runtime is reaped
// regardless of heartbeat state (remote-polecat-execution §9.5).
func TestReapIdlePolecat_MaxRuntimeExceeded(t *testing.T) {
	d, logBuf := maxRuntimeTestDaemon(t, time.Now().Add(-5*time.Hour))

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 4*time.Hour)

	if !strings.Contains(logBuf.String(), "max-runtime-exceeded") {
		t.Errorf("expected max-runtime-exceeded reap, got: %q", logBuf.String())
	}
}

// TestReapIdlePolecat_MaxRuntimeNotExceeded verifies a busy polecat under the
// cap is left alone.
func TestReapIdlePolecat_MaxRuntimeNotExceeded(t *testing.T) {
	d, logBuf := maxRuntimeTestDaemon(t, time.Now().Add(-1*time.Hour))

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 4*time.Hour)

	if strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Errorf("polecat under max_runtime must not be reaped, got: %q", logBuf.String())
	}
}

// TestReapIdlePolecat_NoCapNoReap verifies maxRuntime=0 (no execution block)
// preserves existing behavior: an old but busy polecat is not reaped.
func TestReapIdlePolecat_NoCapNoReap(t *testing.T) {
	d, logBuf := maxRuntimeTestDaemon(t, time.Now().Add(-100*time.Hour))

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 0)

	if strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Errorf("polecat must not be reaped without a cap, got: %q", logBuf.String())
	}
}

// TestRigMaxRuntime verifies cap resolution from rig settings. The hard-kill
// cap engages ONLY when max_runtime is explicitly set: no settings, no
// execution block, and an execution block without max_runtime all yield 0
// (no cap), so merely selecting a backend never silently hard-kills a busy
// polecat.
func TestRigMaxRuntime(t *testing.T) {
	rigPath := t.TempDir()
	settingsDir := filepath.Join(rigPath, "settings")
	_ = os.MkdirAll(settingsDir, 0755)
	settingsPath := filepath.Join(settingsDir, "config.json")
	d := &Daemon{logger: log.New(io.Discard, "", 0)}

	// No settings file at all → no cap.
	if got := d.rigMaxRuntime(rigPath); got != 0 {
		t.Errorf("no settings: rigMaxRuntime = %v, want 0", got)
	}

	writeSettings := func(s *agentconfig.RigSettings) {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(settingsPath, data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Settings without execution block → no cap.
	writeSettings(&agentconfig.RigSettings{Type: "rig-settings", Version: agentconfig.CurrentRigSettingsVersion})
	if got := d.rigMaxRuntime(rigPath); got != 0 {
		t.Errorf("no execution block: rigMaxRuntime = %v, want 0", got)
	}

	// Execution block WITHOUT max_runtime → still no cap (the key safety fix:
	// no silent 4h hard-kill just because a backend/network was selected).
	writeSettings(&agentconfig.RigSettings{
		Type: "rig-settings", Version: agentconfig.CurrentRigSettingsVersion,
		Execution: &agentconfig.ExecutionConfig{Backend: "local"},
	})
	if got := d.rigMaxRuntime(rigPath); got != 0 {
		t.Errorf("execution block without max_runtime: rigMaxRuntime = %v, want 0 (no silent cap)", got)
	}

	// Execution block WITH explicit max_runtime → that value.
	writeSettings(&agentconfig.RigSettings{
		Type: "rig-settings", Version: agentconfig.CurrentRigSettingsVersion,
		Execution: &agentconfig.ExecutionConfig{MaxRuntimeStr: "2h"},
	})
	if got := d.rigMaxRuntime(rigPath); got != 2*time.Hour {
		t.Errorf("explicit: rigMaxRuntime = %v, want 2h", got)
	}
}
