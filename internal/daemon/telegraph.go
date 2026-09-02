package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
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

	mu          sync.Mutex
	process     *os.Process
	selfStarted bool // true iff m.process was assigned by startLocked, not adopted from the PID file
	startedAt   time.Time

	// reaped records whether the current self-started child (m.process, while
	// m.selfStarted) has been observed to exit via cmd.Wait, independent of any
	// platform liveness probe. isProcessAlive alone cannot detect this on
	// Windows, where it re-derives liveness from the PID number and so reports
	// a reaped-but-recycled PID as alive again. It is allocated fresh per child
	// in startLocked and closed over by that child's wait goroutine, so a stale
	// goroutine from a previous child can never mark the current child reaped.
	reaped *atomic.Bool

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
	aliveFn   func(*os.Process) bool
}

// isAlive reports whether p is alive, using aliveFn if set for test injection.
func (m *TelegraphServerManager) isAlive(p *os.Process) bool {
	if m.aliveFn != nil {
		return m.aliveFn(p)
	}
	return isProcessAlive(p)
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
//
// It always re-derives liveness from the PID file rather than trusting a
// cached m.process handle: a handle only encodes a PID number, so once the
// process it originally pointed at exits, that PID can be reused by an
// unrelated process (or, on some platforms, briefly linger as a zombie) and
// a raw liveness signal against the stale handle would then report a dead
// Telegraph as alive forever, since nothing ever routes back through
// PID-file re-validation. This matters most for an *adopted* process (one
// discovered via the PID file rather than started by this manager instance),
// since there both the initial and every subsequent check rely solely on the
// cached handle.
//
// Two handle-derived signals still take precedence over the file, because
// they are more authoritative than a fresh PID-file probe:
//   - If the file is missing or unreadable but this manager's own handle for
//     the process it started is still alive, trust the handle (startLocked
//     treats a failed PID-file write as non-fatal, so the file's absence
//     does not by itself mean Telegraph died) and repair the file.
//   - If the cached handle — self-started or adopted — for the exact PID the
//     file names reports dead, trust that death report over the file. For a
//     self-started child the reaped flag (set once cmd.Wait returns) is the
//     stronger form of this override: it survives even a platform liveness
//     probe that reports the PID as alive after the OS recycled it to an
//     unrelated process.
func (m *TelegraphServerManager) isRunning() (int, bool) {
	if m.runningFn != nil {
		return m.runningFn()
	}
	pid, alive, err := verifyPIDOwnership(m.pidFile())

	// A live handle for a process this manager itself started outranks the PID
	// file whenever the two disagree (file missing/unparseable, naming a dead
	// PID, or naming a different live PID): the file is 0644 in a 0755 dir and
	// verifyPIDOwnership does not authenticate its writer, so any same-uid
	// process can overwrite it. Trusting the file over a disagreeing
	// self-started handle would let a foreign writer redirect Stop()'s kill
	// signal, or spawn a duplicate Telegraph while ours is still alive. An
	// adopted (non-self-started) handle keeps only a weaker form of this:
	// while it stays alive it is likewise never dropped or retargeted on the
	// file's say-so (the file already chose this PID once, at adoption;
	// honoring every later rewrite would let an unauthenticated writer re-aim
	// Stop()'s kill signal at will), but a PID whose only provenance is "a
	// file said so" is never re-blessed with a fresh nonce — the disagreeing
	// file is left as-is, not repaired.
	if m.selfStarted && m.process != nil && m.process.Pid != pid &&
		(m.reaped == nil || !m.reaped.Load()) && m.isAlive(m.process) {
		if _, werr := writePIDFile(m.pidFile(), m.process.Pid); werr != nil {
			m.logger("Telegraph: warning: failed to repair PID file: %v", werr)
		}
		return m.process.Pid, true
	}

	if err != nil || pid == 0 {
		// The file is missing or unparseable — that is not the file naming a
		// specific dead process, just the file being unreadable, so it is not
		// evidence that a handle we already hold is dead, including an
		// adopted one (the file is unauthenticated; see doc above). Keep
		// reporting it running without rewriting the file, so an adopted PID
		// is never re-blessed with a fresh nonce. A self-started child that
		// has already been reaped is the exception: that death report can't
		// be spoofed by the file going missing, so it still outranks the
		// platform's raw liveness probe here (see reaped doc above).
		reaped := m.selfStarted && m.reaped != nil && m.reaped.Load()
		if m.process != nil && !reaped && m.isAlive(m.process) {
			return m.process.Pid, true
		}
		m.process = nil
		m.selfStarted = false
		m.reaped = nil
		return 0, false
	}
	if !alive {
		// Unlike the case above, the file names a specific PID and that PID
		// is confirmed dead — trust that death report over any stale cached
		// handle, which may point at an unrelated process after PID reuse.
		_ = os.Remove(m.pidFile())
		m.process = nil
		m.selfStarted = false
		m.reaped = nil
		return 0, false
	}
	if m.process != nil && m.process.Pid == pid {
		// Once a self-started child has been reaped (cmd.Wait returned), that
		// report is authoritative even if isAlive's platform probe cannot tell
		// the reaped PID apart from a recycled one (see reaped doc above).
		reaped := m.selfStarted && m.reaped != nil && m.reaped.Load()
		if reaped || !m.isAlive(m.process) {
			_ = os.Remove(m.pidFile())
			m.process = nil
			m.selfStarted = false
			m.reaped = nil
			return 0, false
		}
	}
	if m.process == nil || m.process.Pid != pid {
		if m.process != nil && !m.selfStarted && m.isAlive(m.process) {
			// Do not retarget an already-adopted live handle just because the
			// unauthenticated file started naming a different PID.
			return m.process.Pid, true
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			m.process = nil
			m.selfStarted = false
			m.reaped = nil
			return 0, false
		}
		m.process = process
		m.selfStarted = false
		m.reaped = nil
	}
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

	pid := cmd.Process.Pid
	reaped := &atomic.Bool{}
	m.reaped = reaped

	go func() {
		_ = cmd.Wait()
		// Closes over this child's own reaped flag, not a PID number, so a
		// goroutine left over from a previous child can never mark the
		// current child (or a later one that reused its PID) reaped.
		reaped.Store(true)
		_ = logFile.Close()
	}()

	m.process = cmd.Process
	m.selfStarted = true
	m.startedAt = m.now()

	if _, err := writePIDFile(m.pidFile(), pid); err != nil {
		m.logger("Telegraph: warning: failed to write PID file: %v", err)
	}

	m.logger("Telegraph: started subprocess (PID %d), config=%s", pid, cfgPath)
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
	m.selfStarted = false
	m.reaped = nil
}
