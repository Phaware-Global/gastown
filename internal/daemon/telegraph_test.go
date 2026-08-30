package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// newTestTelegraphManager creates a TelegraphServerManager with test hooks wired.
func newTestTelegraphManager(t *testing.T, cfg *TelegraphServerConfig) *TelegraphServerManager {
	t.Helper()
	return NewTelegraphServerManager(t.TempDir(), "gt", cfg, func(format string, v ...interface{}) {
		t.Logf("[telegraph] "+format, v...)
	})
}

// TestTelegraph_DisabledByDefault verifies that a nil or disabled config means no start attempt.
func TestTelegraph_DisabledByDefault(t *testing.T) {
	// nil Telegraph config
	m := newTestTelegraphManager(t, nil)
	if m.IsEnabled() {
		t.Fatal("expected IsEnabled()=false for nil config")
	}

	var started bool
	m.startFn = func() error {
		started = true
		return nil
	}

	m.EnsureRunning() // must be a no-op
	if started {
		t.Fatal("EnsureRunning must not start when disabled")
	}

	// Disabled explicitly
	m2 := newTestTelegraphManager(t, &TelegraphServerConfig{Enabled: false})
	if m2.IsEnabled() {
		t.Fatal("expected IsEnabled()=false for Enabled:false")
	}
	m2.startFn = func() error {
		started = true
		return nil
	}
	m2.EnsureRunning()
	if started {
		t.Fatal("EnsureRunning must not start when Enabled:false")
	}
}

// TestTelegraph_StartFailureDoesNotPropagate verifies that a Telegraph startup
// failure is logged and swallowed — EnsureRunning never panics or returns an error
// (it has no return value), and the daemon heartbeat continues unaffected.
func TestTelegraph_StartFailureDoesNotPropagate(t *testing.T) {
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 5,
		RestartWindow:       time.Minute,
	}
	m := newTestTelegraphManager(t, cfg)

	var attempts int32
	m.startFn = func() error {
		atomic.AddInt32(&attempts, 1)
		return os.ErrPermission // simulated startup failure
	}
	m.runningFn = func() (int, bool) { return 0, false }
	m.sleepFn = func(d time.Duration) {} // skip backoff waits

	// EnsureRunning must not panic and must not return an error (void).
	// The call itself is the test: if it panics or hangs, the test fails.
	m.EnsureRunning()

	if atomic.LoadInt32(&attempts) == 0 {
		t.Fatal("expected at least one start attempt")
	}
}

// TestTelegraph_RestartWithoutGastown verifies the restart-isolation property:
// when the supervised process dies, EnsureRunning restarts only the Telegraph
// subprocess without any wider gastown disruption.
func TestTelegraph_RestartWithoutGastown(t *testing.T) {
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 5,
		RestartWindow:       time.Minute,
	}
	m := newTestTelegraphManager(t, cfg)

	var starts int32
	alive := int32(0) // 0 = dead, 1 = alive

	m.runningFn = func() (int, bool) {
		if atomic.LoadInt32(&alive) == 1 {
			return 1234, true
		}
		return 0, false
	}
	m.startFn = func() error {
		atomic.AddInt32(&starts, 1)
		atomic.StoreInt32(&alive, 1)
		return nil
	}
	// nowFn controls perceived time; start at a fixed point.
	base := time.Now()
	m.nowFn = func() time.Time { return base }

	// First call: process is dead → should start.
	m.EnsureRunning()
	if atomic.LoadInt32(&starts) != 1 {
		t.Fatalf("expected 1 start, got %d", atomic.LoadInt32(&starts))
	}

	// Second call: process is alive → no additional start.
	m.EnsureRunning()
	if atomic.LoadInt32(&starts) != 1 {
		t.Fatalf("expected still 1 start after alive check, got %d", atomic.LoadInt32(&starts))
	}

	// Simulate crash; advance time past the backoff delay so the next EnsureRunning restarts.
	atomic.StoreInt32(&alive, 0)
	base = base.Add(10 * time.Second)

	// Third call: process died → should restart (only Telegraph, not gastown).
	m.EnsureRunning()
	if atomic.LoadInt32(&starts) != 2 {
		t.Fatalf("expected 2 starts after simulated crash, got %d", atomic.LoadInt32(&starts))
	}
}

// TestTelegraph_RestartCapPreventsStorm verifies that crash-looping Telegraph
// does not hammer the system: after MaxRestartsInWindow attempts the manager
// stops retrying within that window.
func TestTelegraph_RestartCapPreventsStorm(t *testing.T) {
	window := 10 * time.Minute
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 3,
		RestartWindow:       window,
	}
	m := newTestTelegraphManager(t, cfg)

	var starts int32
	m.runningFn = func() (int, bool) { return 0, false } // always dead
	m.startFn = func() error {
		atomic.AddInt32(&starts, 1)
		return nil
	}
	m.sleepFn = func(d time.Duration) {}

	// Advance time by 30s per iteration so each call clears the backoff delay,
	// while staying within the 10m restart window to exercise the cap.
	now := time.Now()
	m.nowFn = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		m.EnsureRunning()
		now = now.Add(30 * time.Second)
	}

	got := atomic.LoadInt32(&starts)
	if int(got) > cfg.MaxRestartsInWindow {
		t.Fatalf("expected at most %d starts, got %d", cfg.MaxRestartsInWindow, got)
	}
}

// TestTelegraph_IsPatrolEnabled verifies IsPatrolEnabled wiring for "telegraph".
func TestTelegraph_IsPatrolEnabled(t *testing.T) {
	// nil config → disabled
	if IsPatrolEnabled(nil, "telegraph") {
		t.Fatal("expected false for nil config")
	}

	// config with nil Patrols → disabled
	cfg := &DaemonPatrolConfig{}
	if IsPatrolEnabled(cfg, "telegraph") {
		t.Fatal("expected false for nil Patrols")
	}

	// config with nil Telegraph → disabled
	cfg.Patrols = &PatrolsConfig{}
	if IsPatrolEnabled(cfg, "telegraph") {
		t.Fatal("expected false for nil Telegraph")
	}

	// enabled=false → disabled
	cfg.Patrols.Telegraph = &TelegraphServerConfig{Enabled: false}
	if IsPatrolEnabled(cfg, "telegraph") {
		t.Fatal("expected false for Enabled:false")
	}

	// enabled=true → active
	cfg.Patrols.Telegraph.Enabled = true
	if !IsPatrolEnabled(cfg, "telegraph") {
		t.Fatal("expected true for Enabled:true")
	}
}

// TestTelegraph_ResolvedPaths verifies config path resolution defaults.
func TestTelegraph_ResolvedPaths(t *testing.T) {
	townRoot := "/tmp/test-town"
	cfg := &TelegraphServerConfig{Enabled: true}
	m := NewTelegraphServerManager(townRoot, "gt", cfg, func(string, ...interface{}) {})

	wantCfg := filepath.Join(townRoot, "settings", "telegraph.toml")
	if got := m.resolvedConfigPath(); got != wantCfg {
		t.Errorf("resolvedConfigPath = %q, want %q", got, wantCfg)
	}

	wantLog := filepath.Join(townRoot, "daemon", "telegraph.log")
	if got := m.resolvedLogFile(); got != wantLog {
		t.Errorf("resolvedLogFile = %q, want %q", got, wantLog)
	}

	// Explicit overrides are honoured.
	cfg.ConfigPath = "/custom/telegraph.toml"
	cfg.LogFile = "/var/log/telegraph.log"
	if got := m.resolvedConfigPath(); got != cfg.ConfigPath {
		t.Errorf("resolvedConfigPath with override = %q, want %q", got, cfg.ConfigPath)
	}
	if got := m.resolvedLogFile(); got != cfg.LogFile {
		t.Errorf("resolvedLogFile with override = %q, want %q", got, cfg.LogFile)
	}
}

// TestTelegraph_MissingConfigSkipsStart verifies that when telegraph.toml is absent
// and no override is given, startLocked returns an error (handled non-fatally).
func TestTelegraph_MissingConfigSkipsStart(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		RestartDelay:        0,
		MaxRestartsInWindow: 1,
		RestartWindow:       time.Minute,
	}
	m := NewTelegraphServerManager(tmpDir, "gt", cfg, func(format string, v ...interface{}) {
		t.Logf("[telegraph] "+format, v...)
	})
	m.runningFn = func() (int, bool) { return 0, false }
	m.sleepFn = func(d time.Duration) {}

	// telegraph.toml does not exist in tmpDir/settings → should log and not panic.
	m.EnsureRunning() // must complete without panic
}

// TestTelegraph_StopIsIdempotent verifies Stop() can be called multiple times without panic.
func TestTelegraph_StopIsIdempotent(t *testing.T) {
	cfg := &TelegraphServerConfig{Enabled: true}
	m := newTestTelegraphManager(t, cfg)

	var stopped int32
	m.stopFn = func() { atomic.AddInt32(&stopped, 1) }
	m.runningFn = func() (int, bool) { return 0, false }

	m.Stop()
	m.Stop()
	// stopFn is called for each Stop() invocation (stopLocked delegates to stopFn unconditionally).
	if got := atomic.LoadInt32(&stopped); got != 2 {
		t.Errorf("Stop() called twice: stopFn invocations = %d, want 2", got)
	}
}

// TestTelegraph_StaleProcessHandleDoesNotMaskDeath is a regression test for a
// silent-outage bug: isRunning() used to trust a cached m.process handle's raw
// liveness signal without re-validating it against the PID file. In
// production, a Telegraph process adopted via PID file (not started
// by this manager instance) died mid-run, but the health check kept believing
// it was alive indefinitely — an 8+ minute silent webhook-ingress outage until
// a manual daemon restart. This exercises the real (non-mocked) isRunning()
// path: a stale handle pointing at a live, unrelated process (simulating PID
// reuse / a lingering zombie) must not mask a PID-file-recorded death.
func TestTelegraph_StaleProcessHandleDoesNotMaskDeath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("`true` helper is unix-only")
	}
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 5,
		RestartWindow:       time.Minute,
	}
	m := newTestTelegraphManager(t, cfg)

	if err := os.MkdirAll(filepath.Dir(m.pidFile()), 0755); err != nil {
		t.Fatalf("failed to create pid dir: %v", err)
	}

	// The PID file records a Telegraph process that has already exited. Its
	// PID is reaped but not reserved, so this test depends on the OS not
	// recycling it within the test window.
	deadCmd := exec.Command("true")
	if err := deadCmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	if _, err := writePIDFile(m.pidFile(), deadCmd.Process.Pid); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	// A stale cached handle left over from adoption points at a *different*,
	// still-alive process (the test process itself) — modeling PID reuse
	// after the real Telegraph died.
	m.process = &os.Process{Pid: os.Getpid()}

	var starts int32
	m.startFn = func() error {
		atomic.AddInt32(&starts, 1)
		return nil
	}

	m.EnsureRunning()

	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("expected EnsureRunning to restart Telegraph after the PID-file-recorded process died, got %d starts", got)
	}
}

// TestTelegraph_MissingPIDFileFallsBackToLiveHandle is a regression test:
// startLocked treats a failed writePIDFile as a non-fatal warning, so the
// file can be missing even though the process this manager started is
// alive. isRunning must not treat that as death — doing so would spawn a
// duplicate Telegraph on every health-check tick.
func TestTelegraph_MissingPIDFileFallsBackToLiveHandle(t *testing.T) {
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 5,
		RestartWindow:       time.Minute,
	}
	m := newTestTelegraphManager(t, cfg)
	if err := os.MkdirAll(filepath.Dir(m.pidFile()), 0755); err != nil {
		t.Fatalf("failed to create pid dir: %v", err)
	}

	// No PID file was ever written, but this manager holds a live handle for
	// a process it started itself.
	m.process = &os.Process{Pid: os.Getpid()}
	m.selfStarted = true
	m.startedAt = time.Now()

	var starts int32
	m.startFn = func() error {
		atomic.AddInt32(&starts, 1)
		return nil
	}

	m.EnsureRunning()

	if got := atomic.LoadInt32(&starts); got != 0 {
		t.Fatalf("expected EnsureRunning not to restart a live self-started Telegraph over a missing PID file, got %d starts", got)
	}
	if _, err := os.Stat(m.pidFile()); err != nil {
		t.Fatalf("expected isRunning to repair the missing PID file, stat failed: %v", err)
	}
}

// TestTelegraph_ReapedSelfStartedHandleOverridesRecycledPID is a regression
// test: once a self-started child has been reaped, this manager's own
// handle for it reports death authoritatively. A fresh PID-file probe alone
// can't be trusted to override that, because the OS may have already
// recycled the PID number to an unrelated process by the next health check.
//
// aliveFn stands in for the platform liveness probe so the reaped-handle
// branch is exercised deterministically: a real liveness probe on the PID
// below (the test process itself) would report it alive, masking the branch
// under test the same way it does in production on Windows (see reapedPID).
func TestTelegraph_ReapedSelfStartedHandleOverridesRecycledPID(t *testing.T) {
	cfg := &TelegraphServerConfig{
		Enabled:             true,
		AutoRestart:         true,
		RestartDelay:        0,
		MaxRestartsInWindow: 5,
		RestartWindow:       time.Minute,
	}
	m := newTestTelegraphManager(t, cfg)
	if err := os.MkdirAll(filepath.Dir(m.pidFile()), 0755); err != nil {
		t.Fatalf("failed to create pid dir: %v", err)
	}

	// A self-started handle whose process has already been reaped.
	pid := os.Getpid()
	m.process = &os.Process{Pid: pid}
	m.selfStarted = true
	m.aliveFn = func(*os.Process) bool { return false }

	// The PID file still names that same PID as if it were alive — modeling
	// the OS having recycled it to an unrelated, still-running process
	// before the health check ran.
	if _, err := writePIDFile(m.pidFile(), pid); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	var starts int32
	m.startFn = func() error {
		atomic.AddInt32(&starts, 1)
		return nil
	}

	m.EnsureRunning()

	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("expected EnsureRunning to restart Telegraph when its own reaped handle reports death, got %d starts", got)
	}
	if _, err := os.Stat(m.pidFile()); !os.IsNotExist(err) {
		t.Fatalf("expected isRunning to remove the PID file for the reaped handle, stat err=%v", err)
	}
}

// TestTelegraph_HealthCheckIntervalDefault verifies fallback to 30s.
func TestTelegraph_HealthCheckIntervalDefault(t *testing.T) {
	m := newTestTelegraphManager(t, &TelegraphServerConfig{Enabled: true})
	if got := m.HealthCheckInterval(); got != defaultTelegraphHealthCheckInterval {
		t.Errorf("HealthCheckInterval = %v, want %v", got, defaultTelegraphHealthCheckInterval)
	}

	m2 := newTestTelegraphManager(t, &TelegraphServerConfig{Enabled: true, HealthCheckInterval: 5 * time.Second})
	if got := m2.HealthCheckInterval(); got != 5*time.Second {
		t.Errorf("HealthCheckInterval with override = %v, want 5s", got)
	}
}
