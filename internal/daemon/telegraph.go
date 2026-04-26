package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

const defaultTelegraphHealthCheckInterval = 30 * time.Second

// TelegraphServerManager supervises the Telegraph webhook bridge subprocess.
// It starts `gt telegraph start` as a child process and restarts it on crash,
// providing restart isolation: Telegraph can be restarted without touching gastown.
type TelegraphServerManager struct {
	config   *TelegraphServerConfig
	townRoot string
	gtPath   string
	logger   func(format string, v ...interface{})

	mu        sync.Mutex
	process   *os.Process
	startedAt time.Time

	// Backoff state
	currentDelay time.Duration
	restartTimes []time.Time
	escalated    bool

	// Test hooks
	startFn   func() error
	runningFn func() (int, bool)
	stopFn    func()
	sleepFn   func(time.Duration)
	nowFn     func() time.Time
}

// NewTelegraphServerManager creates a new Telegraph subprocess supervisor.
func NewTelegraphServerManager(townRoot, gtPath string, config *TelegraphServerConfig, logger func(format string, v ...interface{})) *TelegraphServerManager {
	return &TelegraphServerManager{
		config:   config,
		townRoot: townRoot,
		gtPath:   gtPath,
		logger:   logger,
	}
}

// IsEnabled returns whether Telegraph supervision is enabled.
func (m *TelegraphServerManager) IsEnabled() bool {
	return m.config != nil && m.config.Enabled
}

func (m *TelegraphServerManager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

func (m *TelegraphServerManager) doSleep(d time.Duration) {
	if m.sleepFn != nil {
		m.sleepFn(d)
		return
	}
	time.Sleep(d)
}

// HealthCheckInterval returns the configured interval, falling back to default.
func (m *TelegraphServerManager) HealthCheckInterval() time.Duration {
	if m.config != nil && m.config.HealthCheckInterval > 0 {
		return m.config.HealthCheckInterval
	}
	return defaultTelegraphHealthCheckInterval
}

// pidFile returns the path to the Telegraph PID file.
func (m *TelegraphServerManager) pidFile() string {
	return filepath.Join(m.townRoot, "daemon", "telegraph.pid")
}

// resolvedConfigPath returns the telegraph.toml path to use.
func (m *TelegraphServerManager) resolvedConfigPath() string {
	if m.config.ConfigPath != "" {
		return m.config.ConfigPath
	}
	return telegraph.DefaultPath(m.townRoot)
}

// resolvedLogFile returns the log file path to use.
func (m *TelegraphServerManager) resolvedLogFile() string {
	if m.config.LogFile != "" {
		return m.config.LogFile
	}
	return filepath.Join(m.townRoot, "daemon", "telegraph.log")
}

// isRunning checks if the supervised Telegraph process is alive.
// Must be called with m.mu held.
func (m *TelegraphServerManager) isRunning() (int, bool) {
	if m.runningFn != nil {
		return m.runningFn()
	}
	if m.process != nil {
		if isProcessAlive(m.process) {
			return m.process.Pid, true
		}
		m.process = nil
	}
	pid, alive, err := verifyPIDOwnership(m.pidFile())
	if err != nil || pid == 0 || !alive {
		if pid > 0 {
			_ = os.Remove(m.pidFile())
		}
		return 0, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	m.process = process
	return pid, true
}

// EnsureRunning checks whether Telegraph is alive and starts/restarts it if not.
// A startup failure is logged but never returned to the caller — Telegraph failures
// must not interrupt the gastown daemon startup or heartbeat.
func (m *TelegraphServerManager) EnsureRunning() {
	if !m.IsEnabled() {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, running := m.isRunning(); running {
		// If discovered via PID file (process not started by this manager instance),
		// mark startedAt so AutoRestart=false correctly blocks restarts after a crash.
		if m.startedAt.IsZero() {
			m.startedAt = m.now()
		}
		// Reset backoff if the process has been running stably beyond the restart window.
		if m.now().Sub(m.startedAt) > m.restartWindow() {
			m.currentDelay = 0
			m.escalated = false
		}
		return
	}

	// If auto_restart is disabled, only allow the initial start (startedAt zero means never started).
	if !m.config.AutoRestart && !m.startedAt.IsZero() {
		return
	}

	if err := m.restartWithBackoff(); err != nil {
		m.logger("Telegraph: could not start subprocess: %v", err)
	}
}

// restartWithBackoff applies exponential backoff and a restart cap.
// Must be called with m.mu held.
func (m *TelegraphServerManager) restartWithBackoff() error {
	now := m.now()
	m.pruneRestartTimes(now)

	maxRestarts := m.config.MaxRestartsInWindow
	if maxRestarts <= 0 {
		maxRestarts = 5
	}
	if len(m.restartTimes) >= maxRestarts {
		if !m.escalated {
			m.escalated = true
			m.logger("Telegraph: restart cap reached (%d in %v), will not retry until window expires",
				len(m.restartTimes), m.restartWindow())
		}
		return fmt.Errorf("telegraph restart cap exceeded (%d restarts in %v)",
			len(m.restartTimes), m.restartWindow())
	}

	// Instead of sleeping (which would block the daemon loop), check whether the
	// required backoff delay has elapsed since the last restart. If not, return
	// and let the next health-check cycle retry.
	delay := m.backoffDelay()
	if delay > 0 && len(m.restartTimes) > 0 {
		lastRestart := m.restartTimes[len(m.restartTimes)-1]
		if now.Sub(lastRestart) < delay {
			return nil
		}
	}

	m.restartTimes = append(m.restartTimes, m.now())
	m.advanceBackoff()
	return m.startLocked()
}

func (m *TelegraphServerManager) restartWindow() time.Duration {
	if m.config.RestartWindow > 0 {
		return m.config.RestartWindow
	}
	return 10 * time.Minute
}

func (m *TelegraphServerManager) backoffDelay() time.Duration {
	if m.currentDelay <= 0 {
		base := m.config.RestartDelay
		if base <= 0 {
			base = 5 * time.Second
		}
		return base
	}
	return m.currentDelay
}

func (m *TelegraphServerManager) advanceBackoff() {
	base := m.config.RestartDelay
	if base <= 0 {
		base = 5 * time.Second
	}
	maxD := m.config.MaxRestartDelay
	if maxD <= 0 {
		maxD = 5 * time.Minute
	}
	if m.currentDelay <= 0 {
		// First restart: use base delay without doubling so RestartDelay is the actual first wait.
		m.currentDelay = base
		return
	}
	m.currentDelay *= 2
	if m.currentDelay > maxD {
		m.currentDelay = maxD
	}
}

func (m *TelegraphServerManager) pruneRestartTimes(now time.Time) {
	window := m.restartWindow()
	cutoff := now.Add(-window)
	pruned := m.restartTimes[:0]
	for _, t := range m.restartTimes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	m.restartTimes = pruned
}

// startLocked starts the Telegraph subprocess. Must be called with m.mu held.
func (m *TelegraphServerManager) startLocked() error {
	if m.startFn != nil {
		return m.startFn()
	}

	if _, running := m.isRunning(); running {
		return nil
	}

	// Verify telegraph.toml exists before attempting start.
	cfgPath := m.resolvedConfigPath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("telegraph.toml not found at %s: %w", cfgPath, err)
	}

	logPath := m.resolvedLogFile()
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("creating telegraph log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening telegraph log file: %w", err)
	}

	gtBin := m.gtPath
	if gtBin == "" {
		gtBin = "gt"
	}

	args := []string{"telegraph", "start", "--town-root", m.townRoot, "--config", cfgPath}
	cmd := exec.Command(gtBin, args...) //nolint:gosec // G204: args constructed internally
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("starting telegraph subprocess: %w", err)
	}

	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()

	m.process = cmd.Process
	m.startedAt = m.now()

	if _, err := writePIDFile(m.pidFile(), cmd.Process.Pid); err != nil {
		m.logger("Telegraph: warning: failed to write PID file: %v", err)
	}

	m.logger("Telegraph: started subprocess (PID %d), config=%s", cmd.Process.Pid, cfgPath)
	return nil
}

// Stop terminates the Telegraph subprocess.
func (m *TelegraphServerManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// stopLocked terminates Telegraph. Must be called with m.mu held.
func (m *TelegraphServerManager) stopLocked() {
	if m.stopFn != nil {
		m.stopFn()
		return
	}
	pid, running := m.isRunning()
	if !running {
		return
	}
	m.logger("Telegraph: stopping subprocess (PID %d)...", pid)

	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := sendTermSignal(process); err != nil {
		m.logger("Telegraph: warning: failed to send SIGTERM: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			if !isProcessAlive(process) {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	select {
	case <-done:
		m.logger("Telegraph: subprocess stopped gracefully")
	case <-time.After(10 * time.Second):
		m.logger("Telegraph: subprocess did not stop gracefully, forcing termination")
		_ = sendKillSignal(process)
		// Give SIGKILL a moment to take effect before clearing state.
		// If the process is still alive after SIGKILL (extremely rare), we accept
		// the stale state; the next EnsureRunning cycle will re-detect via isRunning.
		time.Sleep(100 * time.Millisecond)
	}

	_ = os.Remove(m.pidFile())
	m.process = nil
}
