package daemon

import (
	"io"
	"log"
	"testing"
	"time"
)

// TestIsPatrolEnabled_PluginSyncDefaultsOn is the regression guard for
// gt-2ea1: plugin_sync must default to enabled (like the reviewer patrol),
// not opt-in — a town that never configured it is exactly the town where a
// plugin's deployed copy can drift silently forever.
func TestIsPatrolEnabled_PluginSyncDefaultsOn(t *testing.T) {
	if !IsPatrolEnabled(nil, "plugin_sync") {
		t.Error("expected plugin_sync enabled by default with nil config")
	}

	empty := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if !IsPatrolEnabled(empty, "plugin_sync") {
		t.Error("expected plugin_sync enabled by default with empty patrols config")
	}
}

func TestIsPatrolEnabled_PluginSyncCanBeDisabled(t *testing.T) {
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		PluginSync: &PatrolConfig{Enabled: false},
	}}
	if IsPatrolEnabled(cfg, "plugin_sync") {
		t.Error("expected plugin_sync disabled when explicitly configured off")
	}
}

func TestPluginSyncInterval_DefaultAndConfigured(t *testing.T) {
	if got := pluginSyncInterval(nil); got != defaultPluginSyncInterval {
		t.Errorf("expected default interval %v, got %v", defaultPluginSyncInterval, got)
	}

	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		PluginSync: &PatrolConfig{Enabled: true, Interval: "10m"},
	}}
	if got := pluginSyncInterval(cfg); got != 10*time.Minute {
		t.Errorf("expected configured interval 10m, got %v", got)
	}
}

func TestPluginSyncInterval_InvalidStringFallsBackToDefault(t *testing.T) {
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		PluginSync: &PatrolConfig{Enabled: true, Interval: "not-a-duration"},
	}}
	if got := pluginSyncInterval(cfg); got != defaultPluginSyncInterval {
		t.Errorf("expected fallback to default on invalid interval, got %v", got)
	}
}

// TestRunPluginSync_SkipsWhenDisabled verifies runPluginSync bails out
// immediately (no panic, no work attempted) when the patrol is disabled —
// it must never fall through to FindGastownGitDir/SyncFromOrigin in that case.
func TestRunPluginSync_SkipsWhenDisabled(t *testing.T) {
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PluginSync: &PatrolConfig{Enabled: false},
		}},
		logger: log.New(io.Discard, "", 0),
	}
	d.runPluginSync() // must return without touching the filesystem or panicking
}

// TestRunPluginSync_SkipsWhenAllowlistEmpty is the regression guard for the
// PR #236 review finding: dolt-archive's deployed copy carries prod fixes
// that are not on origin/main, so syncedPlugins must stay empty until those
// are backported (see its doc comment). This locks in that runPluginSync
// no-ops on an empty allowlist instead of calling FindGastownGitDir/
// SyncFromOrigin — which, with the patrol enabled by default, would
// otherwise run (and fail loudly every cycle) as soon as this test's zero
// values satisfy isPatrolActive.
func TestRunPluginSync_SkipsWhenAllowlistEmpty(t *testing.T) {
	if len(syncedPlugins) != 0 {
		t.Fatalf("expected syncedPlugins to be empty pending gt-bpew backport, got %v", syncedPlugins)
	}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	d.runPluginSync() // must return without touching the filesystem, network, or d.ctx
}
