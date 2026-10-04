package cmd

import (
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// reaperKillSwitchTown builds a town whose mayor/daemon.json holds wispReaper,
// chdirs into it, and points the reaper CLI at a local listener. The returned
// counter reports connections to that listener: every reaper SQL step connects
// first, so zero connections proves no write path ran.
func reaperKillSwitchTown(t *testing.T, wispReaper string) *atomic.Int32 {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	daemonJSON := `{"patrols":{"wisp_reaper":` + wispReaper + `}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte(daemonJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(town)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			c.Close()
		}
	}()

	oldHost, oldPort, oldDB, oldDry, oldDelay := reaperHost, reaperPort, reaperDB, reaperDryRun, reaperDBDelay
	reaperHost, reaperPort, reaperDB, reaperDryRun, reaperDBDelay = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port, "hq", false, "0s"
	t.Cleanup(func() {
		reaperHost, reaperPort, reaperDB, reaperDryRun, reaperDBDelay = oldHost, oldPort, oldDB, oldDry, oldDelay
	})
	return &conns
}

func waitForConns(conns *atomic.Int32, want int32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if conns.Load() >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return conns.Load() >= want
}

var reaperWriteCmds = map[string]*cobra.Command{
	"reap":       reaperReapCmd,
	"purge":      reaperPurgeCmd,
	"auto-close": reaperAutoCloseCmd,
	"run":        reaperRunCmd,
}

// The reaper CLI must refuse to write when the operator has switched the
// patrol off, read live from mayor/daemon.json on every invocation.
func TestReaperWriteCommandsRefuseWhenKillSwitchOn(t *testing.T) {
	for name, cmd := range reaperWriteCmds {
		for _, tc := range []struct{ label, cfg string }{
			{"destructive=false", `{"enabled":true,"destructive":false}`},
			{"enabled=false", `{"enabled":false}`},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				conns := reaperKillSwitchTown(t, tc.cfg)
				if err := cmd.RunE(cmd, nil); err == nil {
					t.Fatal("expected a non-nil error, got nil")
				}
				time.Sleep(150 * time.Millisecond)
				if n := conns.Load(); n != 0 {
					t.Errorf("%d Dolt connections made; no reaper step may run", n)
				}
			})
		}
	}
}

// Positive control: without the switch the same commands reach Dolt, so the
// refusal test above can actually detect a write attempt.
func TestReaperWriteCommandsRunWhenKillSwitchOff(t *testing.T) {
	for name, cmd := range reaperWriteCmds {
		t.Run(name, func(t *testing.T) {
			conns := reaperKillSwitchTown(t, `{"enabled":true,"destructive":true}`)
			_ = cmd.RunE(cmd, nil)
			if !waitForConns(conns, 1, 2*time.Second) {
				t.Error("command never reached Dolt with the kill switch off")
			}
		})
	}
}

// --dry-run writes nothing, so it stays allowed while the switch is on.
func TestReaperDryRunAllowedWhenKillSwitchOn(t *testing.T) {
	for name, cmd := range reaperWriteCmds {
		t.Run(name, func(t *testing.T) {
			conns := reaperKillSwitchTown(t, `{"enabled":true,"destructive":false}`)
			reaperDryRun = true
			_ = cmd.RunE(cmd, nil)
			if !waitForConns(conns, 1, 2*time.Second) {
				t.Error("dry-run was blocked by the kill switch")
			}
		})
	}
}

// The switch is read on every invocation: flipping daemon.json takes effect
// without restarting anything.
func TestReaperKillSwitchIsReadLive(t *testing.T) {
	conns := reaperKillSwitchTown(t, `{"enabled":true,"destructive":true}`)
	_ = reaperReapCmd.RunE(reaperReapCmd, nil)
	if !waitForConns(conns, 1, 2*time.Second) {
		t.Fatal("first run should reach Dolt")
	}
	before := conns.Load()

	flipped := `{"patrols":{"wisp_reaper":{"enabled":true,"destructive":false}}}`
	if err := os.WriteFile(filepath.Join("mayor", "daemon.json"), []byte(flipped), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reaperReapCmd.RunE(reaperReapCmd, nil); err == nil {
		t.Fatal("run after flipping destructive=false must fail")
	}
	time.Sleep(150 * time.Millisecond)
	if n := conns.Load(); n != before {
		t.Errorf("connections rose %d -> %d after the flip", before, n)
	}
}

// An unreadable switch must not be read as "off": fail closed.
func TestReaperRefusesWhenDaemonConfigUnparseable(t *testing.T) {
	conns := reaperKillSwitchTown(t, `{"enabled":true}`)
	if err := os.WriteFile(filepath.Join("mayor", "daemon.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reaperReapCmd.RunE(reaperReapCmd, nil); err == nil {
		t.Fatal("expected an error for an unparseable daemon.json")
	}
	time.Sleep(150 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Errorf("%d Dolt connections made with an unreadable kill switch", n)
	}
}

func writeTownSettings(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll("settings", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("settings", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The town-level disabled_patrols list switches the reaper off even when
// daemon.json enables it, matching the daemon's isPatrolActive.
func TestReaperWriteCommandsRefuseWhenTownDisablesWispReaper(t *testing.T) {
	for name, cmd := range reaperWriteCmds {
		t.Run(name, func(t *testing.T) {
			conns := reaperKillSwitchTown(t, `{"enabled":true,"destructive":true}`)
			writeTownSettings(t, `{"disabled_patrols":["doctor_dog","wisp_reaper"]}`)
			if err := cmd.RunE(cmd, nil); err == nil {
				t.Fatal("expected a non-nil error, got nil")
			}
			time.Sleep(150 * time.Millisecond)
			if n := conns.Load(); n != 0 {
				t.Errorf("%d Dolt connections made; no reaper step may run", n)
			}
		})
	}
}

// Other patrols in disabled_patrols must not block the reaper.
func TestReaperRunsWhenTownDisablesOnlyOtherPatrols(t *testing.T) {
	conns := reaperKillSwitchTown(t, `{"enabled":true,"destructive":true}`)
	writeTownSettings(t, `{"disabled_patrols":["doctor_dog"]}`)
	_ = reaperReapCmd.RunE(reaperReapCmd, nil)
	if !waitForConns(conns, 1, 2*time.Second) {
		t.Error("reap was blocked by an unrelated disabled patrol")
	}
}

// A settings file that exists but cannot be parsed fails closed.
func TestReaperRefusesWhenTownSettingsUnparseable(t *testing.T) {
	conns := reaperKillSwitchTown(t, `{"enabled":true,"destructive":true}`)
	writeTownSettings(t, "{not json")
	if err := reaperReapCmd.RunE(reaperReapCmd, nil); err == nil {
		t.Fatal("expected an error for unparseable town settings")
	}
	time.Sleep(150 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Errorf("%d Dolt connections made with unreadable town settings", n)
	}
}
