package daemon

import (
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestWispReaperInterval(t *testing.T) {
	// Default (now 1h after Dog-driven refactor)
	if got := wispReaperInterval(nil); got != defaultWispReaperInterval {
		t.Errorf("expected default %v, got %v", defaultWispReaperInterval, got)
	}

	// Custom
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:     true,
				IntervalStr: "2h",
			},
		},
	}
	if got := wispReaperInterval(config); got != 2*time.Hour {
		t.Errorf("expected 2h, got %v", got)
	}

	// Invalid falls back to default
	config.Patrols.WispReaper.IntervalStr = "nope"
	if got := wispReaperInterval(config); got != defaultWispReaperInterval {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

func TestWispReaperMaxAge(t *testing.T) {
	if got := wispReaperMaxAge(nil); got != defaultWispMaxAge {
		t.Errorf("expected default %v, got %v", defaultWispMaxAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:   true,
				MaxAgeStr: "48h",
			},
		},
	}
	if got := wispReaperMaxAge(config); got != 48*time.Hour {
		t.Errorf("expected 48h, got %v", got)
	}
}

func TestWispDeleteAge(t *testing.T) {
	if got := wispDeleteAge(nil); got != defaultWispDeleteAge {
		t.Errorf("expected default %v, got %v", defaultWispDeleteAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:      true,
				DeleteAgeStr: "336h",
			},
		},
	}
	if got := wispDeleteAge(config); got != 14*24*time.Hour {
		t.Errorf("expected 336h, got %v", got)
	}
}

func TestDefaultReaperIntervalIsOneHour(t *testing.T) {
	// Verify the default changed from 30m to 1h per issue gt-caf7.
	if defaultWispReaperInterval != 1*time.Hour {
		t.Errorf("expected default interval 1h, got %v", defaultWispReaperInterval)
	}
}

func TestWispStaleIssueAge(t *testing.T) {
	if got := wispStaleIssueAge(nil); got != defaultStaleIssueAge {
		t.Errorf("expected default %v, got %v", defaultStaleIssueAge, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:          true,
				StaleIssueAgeStr: "48h",
			},
		},
	}
	if got := wispStaleIssueAge(config); got != 48*time.Hour {
		t.Errorf("expected 48h, got %v", got)
	}

	// Invalid falls back to default
	config.Patrols.WispReaper.StaleIssueAgeStr = "nope"
	if got := wispStaleIssueAge(config); got != defaultStaleIssueAge {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

// TestDefaultStaleIssueAgeIsThirtyDays guards against gt-73to recurring: the
// daemon's default MUST equal reaper.DefaultStaleIssueAge (720h/30d) — the
// same value documented by mol-dog-reaper.formula.toml and used by
// `gt reaper auto-close --stale-age`. It must never silently drift to a
// shorter value (e.g. the previous 168h/7d bug that auto-closed live issues).
func TestDefaultStaleIssueAgeIsThirtyDays(t *testing.T) {
	if defaultStaleIssueAge != reaper.DefaultStaleIssueAge {
		t.Errorf("defaultStaleIssueAge (%v) must equal reaper.DefaultStaleIssueAge (%v)",
			defaultStaleIssueAge, reaper.DefaultStaleIssueAge)
	}
	if defaultStaleIssueAge != 720*time.Hour {
		t.Errorf("expected default stale issue age 720h (30d), got %v", defaultStaleIssueAge)
	}
}

// TestReapWispsDispatchesThirtyDayStaleIssueAge verifies the dispatch vars
// sent to the Dog carry the 30-day default, not the old 7-day one (gt-73to).
func TestReapWispsDispatchesThirtyDayStaleIssueAge(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{Enabled: true},
		},
	}
	got := wispStaleIssueAge(config)
	want := 720 * time.Hour
	if got != want {
		t.Errorf("expected dispatched stale_issue_age %v, got %v", want, got)
	}
}

// writeFakeBin writes an executable shell script into dir and returns its path.
func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

// newReaperTestDaemon returns a daemon whose gt/bd binaries are fake scripts
// (gt runs gtBody; bd always succeeds) and whose Dolt server port is a local
// listener. The returned counter reports connection attempts to that port: any
// reaper SQL step (discover, schema check, reap, close) must connect first, so
// a zero count proves no reaper step ran.
func newReaperTestDaemon(t *testing.T, wr *WispReaperConfig, gtBody string) (*Daemon, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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

	return &Daemon{
		config:       &Config{TownRoot: dir},
		logger:       log.New(io.Discard, "", 0),
		gtPath:       writeFakeBin(t, dir, "gt", gtBody),
		bdPath:       writeFakeBin(t, dir, "bd", "exit 0"),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{WispReaper: wr}},
		doltServer: &DoltServerManager{
			config: &DoltServerConfig{Port: ln.Addr().(*net.TCPAddr).Port},
		},
	}, &conns
}

// TestReapWispsDispatchFailureRunsNoDestructiveStep guards gt-qlah: when the
// Dog dispatch fails, the daemon must skip the cycle. It previously ran the
// whole reaper (reap, close, auto-close) inline, bypassing every Dog-level hold.
func TestReapWispsDispatchFailureRunsNoDestructiveStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *WispReaperConfig
	}{
		{"explicit databases", &WispReaperConfig{Enabled: true, Databases: []string{"hq"}}},
		{"discovered databases", &WispReaperConfig{Enabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, conns := newReaperTestDaemon(t, tc.cfg, "exit 1")
			d.reapWisps()
			// Connections are accepted asynchronously; give a stray one time to land.
			time.Sleep(200 * time.Millisecond)
			if n := conns.Load(); n != 0 {
				t.Errorf("reapWisps ran reaper SQL steps after dispatch failure (%d connections to Dolt)", n)
			}
		})
	}
}

// TestReapWispsDestructiveSwitch verifies the config switch the mayor can flip
// without a code change: destructive=false forces dry_run on the Dog dispatch.
func TestReapWispsDestructiveSwitch(t *testing.T) {
	off := false
	on := true
	for _, tc := range []struct {
		name        string
		destructive *bool
		wantDryRun  bool
	}{
		{"unset defaults to destructive", nil, false},
		{"explicit true", &on, false},
		{"explicit false forces dry run", &off, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &WispReaperConfig{Enabled: true, Destructive: tc.destructive}
			d, _ := newReaperTestDaemon(t, cfg, "")
			argsFile := filepath.Join(t.TempDir(), "args")
			d.gtPath = writeFakeBin(t, filepath.Dir(argsFile), "gt-rec", `echo "$@" > "`+argsFile+`"`)

			d.reapWisps()

			raw, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("gt sling was not invoked: %v", err)
			}
			got := strings.Contains(string(raw), "dry_run=true")
			if got != tc.wantDryRun {
				t.Errorf("dry_run=true in dispatch args = %v, want %v (args: %s)", got, tc.wantDryRun, raw)
			}
		})
	}
}

// failingDispatchGt returns a gt script that fails `sling` and records every
// `escalate` invocation (one line each) to the returned file.
func failingDispatchGt(t *testing.T, dir string) (gt, escalations string) {
	t.Helper()
	escalations = filepath.Join(dir, "escalations")
	gt = writeFakeBin(t, dir, "gt-esc", `case "$1" in
escalate) echo "$@" >> "`+escalations+`"; exit 0 ;;
*) exit 1 ;;
esac`)
	return gt, escalations
}

func countEscalations(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

// Three consecutive failed dispatches raise exactly one HIGH escalation; later
// failures stay quiet until a dispatch succeeds.
func TestReapWispsEscalatesOnceAfterThreeConsecutiveDispatchFailures(t *testing.T) {
	d, _ := newReaperTestDaemon(t, &WispReaperConfig{Enabled: true}, "exit 1")
	gt, esc := failingDispatchGt(t, t.TempDir())
	d.gtPath = gt

	for i := 1; i <= 2; i++ {
		d.reapWisps()
		if n := countEscalations(t, esc); n != 0 {
			t.Fatalf("after failure %d: %d escalations, want 0", i, n)
		}
	}
	d.reapWisps()
	if n := countEscalations(t, esc); n != 1 {
		t.Fatalf("after failure 3: %d escalations, want 1", n)
	}
	raw, _ := os.ReadFile(esc)
	if !strings.Contains(string(raw), "-s HIGH") {
		t.Errorf("escalation severity is not HIGH: %s", raw)
	}
	d.reapWisps()
	if n := countEscalations(t, esc); n != 1 {
		t.Fatalf("after failure 4: %d escalations, want still 1", n)
	}
}

// A successful dispatch re-arms the escalation.
func TestReapWispsEscalationRearmsAfterSuccessfulDispatch(t *testing.T) {
	d, _ := newReaperTestDaemon(t, &WispReaperConfig{Enabled: true}, "exit 1")
	dir := t.TempDir()
	flag := filepath.Join(dir, "ok")
	esc := filepath.Join(dir, "escalations")
	d.gtPath = writeFakeBin(t, dir, "gt-flip", `case "$1" in
escalate) echo "$@" >> "`+esc+`"; exit 0 ;;
sling) [ -e "`+flag+`" ] && exit 0; exit 1 ;;
esac`)

	for i := 0; i < 3; i++ {
		d.reapWisps()
	}
	if n := countEscalations(t, esc); n != 1 {
		t.Fatalf("first run: %d escalations, want 1", n)
	}
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d.reapWisps() // success resets the streak
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d.reapWisps()
	}
	if n := countEscalations(t, esc); n != 2 {
		t.Fatalf("after re-arm: %d escalations, want 2", n)
	}
}
