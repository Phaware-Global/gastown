package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// writeFakeRefineryBD creates a fake bd script that returns a valid (non-docked)
// rig bead JSON for 'show' calls and an empty array for 'list' calls.
// This allows isRigOperational to succeed without a real Dolt server.
func writeFakeRefineryBD(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    show) IS_SHOW=1 ;;
    list) IS_LIST=1 ;;
    *-rig-*) BEAD_ID="$arg" ;;
  esac
done
if [ "${IS_LIST:-0}" = "1" ]; then
  echo '[]'
  exit 0
fi
if [ "${IS_SHOW:-0}" = "1" ] && [ -n "${BEAD_ID:-}" ]; then
  printf '[{"id":"%s","labels":[],"status":"open","issue_type":"rig"}]\n' "$BEAD_ID"
  exit 0
fi
echo '[]'
exit 0
`
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
}

// writeFakeHungRefineryTmux creates a fake tmux script that simulates a session
// that is alive (agent running) but has had no activity since Unix epoch (stale).
// Logs each command to tmuxLog so the test can assert kill-session was called.
func writeFakeHungRefineryTmux(t *testing.T, dir, tmuxLog string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
# Log to TMUX_LOG if set; otherwise use a fixed path.
LOG_FILE="%s"
printf "%%s\n" "$*" >> "$LOG_FILE"

case "$*" in
  *-V*)            echo "tmux 3.3a"; exit 0;;
  *has-session*)   exit 0;;
  *show-environment*) exit 1;;
  *session_activity*) echo "1";;
  *display-message*) echo "claude";;
  *list-panes*)    printf "claude\t\n"; exit 0;;
  *kill-session*)  exit 0;;
  *new-session*)   exit 0;;
  *set-environment*) exit 0;;
  *send-keys*)     exit 0;;
  *) exit 1;;
esac
`, tmuxLog)
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake hung tmux: %v", err)
	}
}

// writeFakeHealthyRefineryTmux creates a fake tmux script that simulates a session
// that is alive AND has recent activity (current Unix timestamp).
func writeFakeHealthyRefineryTmux(t *testing.T, dir, tmuxLog string) {
	t.Helper()
	// Use the current Unix timestamp so the session looks recently active.
	recentTimestamp := fmt.Sprintf("%d", time.Now().Unix())
	script := fmt.Sprintf(`#!/bin/sh
LOG_FILE="%s"
printf "%%s\n" "$*" >> "$LOG_FILE"

case "$*" in
  *-V*)            echo "tmux 3.3a"; exit 0;;
  *has-session*)   exit 0;;
  *show-environment*) exit 1;;
  *session_activity*) echo "%s";;
  *display-message*) echo "claude";;
  *list-panes*)    printf "claude\t\n"; exit 0;;
  *kill-session*)  exit 0;;
  *new-session*)   exit 0;;
  *set-environment*) exit 0;;
  *send-keys*)     exit 0;;
  *) exit 1;;
esac
`, tmuxLog, recentTimestamp)
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake healthy tmux: %v", err)
	}
}

// setupRefineryZombieTest creates a temp daemon environment for zombie spawn tests.
// Returns townRoot and cleans up the session prefix registry on test exit.
func setupRefineryZombieTest(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()

	// Register a test prefix so session names don't collide with live sessions
	reg := session.NewPrefixRegistry()
	reg.Register("xut", "testrig")
	old := session.DefaultRegistry()
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })

	// Create events dir with a pending event so hasPendingEvents returns true.
	// Without this, ensureRefineryRunning skips spawn if no session is detected healthy.
	eventsDir := filepath.Join(townRoot, "events", "refinery")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatalf("create events dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "pending.event"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("create pending event: %v", err)
	}

	// Create the rig directory so rig.LoadRigConfig doesn't panic on stat
	if err := os.MkdirAll(filepath.Join(townRoot, "testrig"), 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}

	return townRoot
}

// TestEnsureRefineryRunning_ZombieSessionIsReaped verifies that a refinery session
// that is alive (Claude running) but has had no tmux activity for > hung threshold
// is killed before a new session is spawned (regression test for gt-cza incident).
func TestEnsureRefineryRunning_ZombieSessionIsReaped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires bash for fake tmux/bd scripts")
	}

	townRoot := setupRefineryZombieTest(t)
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}

	writeFakeHungRefineryTmux(t, fakeBinDir, tmuxLog)
	writeFakeRefineryBD(t, fakeBinDir)

	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmux(),
	}

	d.ensureRefineryRunning("testrig")

	got := logBuf.String()

	// Verify that the daemon log indicates zombie detection
	if !strings.Contains(got, "zombie") && !strings.Contains(got, "reaping") {
		t.Errorf("expected log to mention zombie/reaping, got: %q", got)
	}

	// Verify kill-session was invoked (the reap itself)
	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	tmuxCalls := string(data)
	if !strings.Contains(tmuxCalls, "kill-session") {
		t.Errorf("expected kill-session in tmux calls for hung refinery, got:\n%s", tmuxCalls)
	}
}

// TestEnsureRefineryRunning_HealthySessionNotReaped verifies that a refinery session
// that is alive and has had recent tmux activity is NOT killed (healthy sessions
// must not be "serial-killed" by the zombie-detection gate).
func TestEnsureRefineryRunning_HealthySessionNotReaped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires bash for fake tmux/bd scripts")
	}

	townRoot := setupRefineryZombieTest(t)
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}

	writeFakeHealthyRefineryTmux(t, fakeBinDir, tmuxLog)
	writeFakeRefineryBD(t, fakeBinDir)

	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmux(),
	}

	d.ensureRefineryRunning("testrig")

	got := logBuf.String()

	// Healthy session must produce "healthy, skipping spawn" — not "zombie"
	if strings.Contains(got, "zombie") || strings.Contains(got, "reaping") {
		t.Errorf("healthy session must not be reaped, got: %q", got)
	}
	if !strings.Contains(got, "healthy") {
		t.Errorf("expected log to mention healthy session, got: %q", got)
	}

	// Verify kill-session was NOT invoked
	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	tmuxCalls := string(data)
	if strings.Contains(tmuxCalls, "kill-session") {
		t.Errorf("kill-session must not be called for healthy refinery, tmux calls:\n%s", tmuxCalls)
	}
}
