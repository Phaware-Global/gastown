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

	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
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

// TestEnsureRefineryRunning_IdleSilentSessionNotReaped is the gt-pzf serial-killer
// regression: a refinery that is process-alive but silent past the hung threshold
// must NOT be reaped when the merge queue is empty and no wake event is pending.
// An idle refinery waiting on await-event legitimately produces no tmux output, so
// reaping it for silence killed healthy sessions every ~threshold ("5 deaths in
// <20hrs"). Only a silent session WITH pending work is genuinely stuck.
func TestEnsureRefineryRunning_IdleSilentSessionNotReaped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires bash for fake tmux/bd scripts")
	}

	townRoot := setupRefineryZombieTest(t)
	// Remove the pending event so hasPendingEvents is false; the fake bd returns
	// an empty queue, so refineryHasWork reports no work — the idle case.
	if err := os.Remove(filepath.Join(townRoot, "events", "refinery", "pending.event")); err != nil {
		t.Fatalf("remove pending event: %v", err)
	}

	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}

	// Hung tmux (silent since epoch) + empty-queue bd = idle-but-silent session.
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
	if strings.Contains(got, "reaping for respawn") {
		t.Errorf("idle silent refinery must not be reaped (serial-killer), got: %q", got)
	}
	if !strings.Contains(got, "not reaping") {
		t.Errorf("expected log to note the idle session was left running, got: %q", got)
	}

	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	if strings.Contains(string(data), "kill-session") {
		t.Errorf("kill-session must not be called for an idle refinery with an empty queue, tmux calls:\n%s", string(data))
	}
}

// writeFakeQueuedMRBD writes a fake bd that reports a non-empty durable merge
// queue: 'bd sql' (the merge-request wisp query used by Manager.Queue) returns
// one open gt:merge-request row, 'bd show' returns a non-docked rig bead, and
// 'bd list' returns empty. This drives refineryHasWork's durable-queue branch.
func writeFakeQueuedMRBD(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
SUB=""
for arg in "$@"; do
  case "$arg" in
    -*) ;;
    sql)  SUB=sql;  break ;;
    list) SUB=list; break ;;
    show) SUB=show; break ;;
  esac
done
for arg in "$@"; do
  case "$arg" in *-rig-*) BEAD_ID="$arg" ;; esac
done
case "$SUB" in
  sql)
    echo '[{"id":"xut-wisp-mr1","title":"Merge PR #1","description":"","status":"open","priority":2,"assignee":"","created_at":"2026-07-05 10:00:00","updated_at":"2026-07-05 10:00:00","created_by":"polecat","labels_csv":"gt:merge-request"}]'
    exit 0 ;;
  show)
    if [ -n "${BEAD_ID:-}" ]; then
      printf '[{"id":"%s","labels":[],"status":"open","issue_type":"rig"}]\n' "$BEAD_ID"
    else
      echo '[]'
    fi
    exit 0 ;;
  *)
    echo '[]'
    exit 0 ;;
esac
`
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake queued-MR bd: %v", err)
	}
}

// TestRefineryHasWork is the gt-pzf durable-queue regression: the daemon must
// treat a merge-request queued in beads as work even when no transient .event
// file is pending, so a down refinery with a queued MR is not stranded.
func TestRefineryHasWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires bash for fake bd scripts")
	}

	newDaemon := func(townRoot string) *Daemon {
		return &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(&strings.Builder{}, "", 0)}
	}
	newMgr := func(townRoot string) *refinery.Manager {
		return refinery.NewManager(&rig.Rig{Name: "testrig", Path: filepath.Join(townRoot, "testrig")})
	}

	t.Run("pending event counts as work", func(t *testing.T) {
		townRoot := setupRefineryZombieTest(t) // leaves a pending.event in place
		fakeBinDir := t.TempDir()
		writeFakeRefineryBD(t, fakeBinDir) // empty queue
		t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if !newDaemon(townRoot).refineryHasWork(newMgr(townRoot), "testrig") {
			t.Error("expected a pending .event to count as work")
		}
	})

	t.Run("empty queue and no events is no work", func(t *testing.T) {
		townRoot := setupRefineryZombieTest(t)
		if err := os.Remove(filepath.Join(townRoot, "events", "refinery", "pending.event")); err != nil {
			t.Fatalf("remove pending event: %v", err)
		}
		fakeBinDir := t.TempDir()
		writeFakeRefineryBD(t, fakeBinDir) // empty queue
		t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if newDaemon(townRoot).refineryHasWork(newMgr(townRoot), "testrig") {
			t.Error("expected no work with an empty queue and no pending events")
		}
	})

	t.Run("queued MR counts as work without any event", func(t *testing.T) {
		townRoot := setupRefineryZombieTest(t)
		if err := os.Remove(filepath.Join(townRoot, "events", "refinery", "pending.event")); err != nil {
			t.Fatalf("remove pending event: %v", err)
		}
		fakeBinDir := t.TempDir()
		writeFakeQueuedMRBD(t, fakeBinDir) // non-empty durable queue
		t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if !newDaemon(townRoot).refineryHasWork(newMgr(townRoot), "testrig") {
			t.Error("expected a queued merge-request to count as work with no pending event (gt-pzf)")
		}
	})
}
