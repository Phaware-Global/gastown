package witness

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

// stubRestartSeams replaces the hook lookup and the restart and stop calls,
// returning counters for the latter two. Callers must not be parallel.
func stubRestartSeams(t *testing.T, hooked []*beads.Issue, lookupErr error) (restarts, stops *int) {
	t.Helper()
	oldList, oldRestart, oldStop, oldVerify := listAssignedWork, restartPolecatSession, stopPolecatSession, verifyBranchAlreadyMerged
	t.Cleanup(func() {
		listAssignedWork, restartPolecatSession, stopPolecatSession, verifyBranchAlreadyMerged = oldList, oldRestart, oldStop, oldVerify
	})
	restarts, stops = new(int), new(int)
	listAssignedWork = func(_, _, _, status string) ([]*beads.Issue, error) {
		if status != beads.StatusHooked {
			return nil, lookupErr
		}
		return hooked, lookupErr
	}
	restartPolecatSession = func(_, _, _ string) error { *restarts++; return nil }
	stopPolecatSession = func(_, _, _ string) error { *stops++; return nil }
	verifyBranchAlreadyMerged = func(_, _, _ string) (bool, error) { return false, nil }
	return restarts, stops
}

// dirtyPolecatWorktree creates a polecat worktree holding an uncommitted file.
func dirtyPolecatWorktree(t *testing.T, townRoot string) {
	t.Helper()
	clone := filepath.Join(townRoot, "testrig", "polecats", "scavenger", "testrig")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(clone, "work.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Not parallel: overrides package-level seams.
func TestDetectZombieDeadSession_RestartsOnlyWithWork(t *testing.T) {
	hooked := []*beads.Issue{{ID: "tr-1", Status: beads.StatusHooked}}
	tests := []struct {
		name      string
		hooked    []*beads.Issue
		lookupErr error
		dirty     bool
		want      int
	}{
		{name: "empty hook", want: 0},
		{name: "hooked bead", hooked: hooked, want: 1},
		{name: "uncommitted work", dirty: true, want: 1},
		{name: "hook unreadable", lookupErr: errors.New("db down"), want: 1},
	}
	intents := map[string]*DoneIntent{
		"active state": nil,
		"done intent":  {ExitType: "COMPLETED", Timestamp: time.Now().Add(-time.Hour)},
	}
	for path, intent := range intents {
		for _, tt := range tests {
			t.Run(path+"/"+tt.name, func(t *testing.T) {
				installFakeTmuxNoServer(t)
				restarts, stops := stubRestartSeams(t, tt.hooked, tt.lookupErr)
				townRoot := t.TempDir()
				if tt.dirty {
					dirtyPolecatWorktree(t, townRoot)
				}
				bd, _ := mockBd(
					func([]string) (string, error) { return "[]", nil },
					func([]string) error { return nil },
				)

				_, found := detectZombieDeadSession(bd, townRoot, townRoot, "testrig", "scavenger", "tr-scavenger",
					tmux.NewTmux(), intent, time.Now(), config.DefaultOperationalConfig().GetWitnessConfig(),
					&agentBeadSnapshot{AgentState: "working"}, DetectZombieOptions{})

				if *restarts != tt.want {
					t.Errorf("restarts = %d, want %d", *restarts, tt.want)
				}
				if found != (tt.want == 1) {
					t.Errorf("found = %v, want %v", found, tt.want == 1)
				}
				if *stops != 0 {
					t.Errorf("stops = %d, want 0 for an already dead session", *stops)
				}
			})
		}
	}
}

// A session that is still up but has nothing to do is stopped, not restarted.
//
// Not parallel: overrides package-level seams.
func TestRestartOrStop(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		hooked                  []*beads.Issue
		wantRestarts, wantStops int
		wantActive              bool
	}{
		{name: "empty hook", wantStops: 1},
		{name: "hooked bead", hooked: []*beads.Issue{{ID: "tr-1"}}, wantRestarts: 1, wantActive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			restarts, stops := stubRestartSeams(t, tt.hooked, nil)
			bd, _ := mockBd(
				func([]string) (string, error) { return "[]", nil },
				func([]string) error { return nil },
			)
			townRoot := t.TempDir()
			z := &ZombieResult{PolecatName: "scavenger", WasActive: true, Classification: ZombieAgentDeadInSession}

			restartOrStop(bd, townRoot, townRoot, "testrig", "scavenger", z, DetectZombieOptions{})

			if *restarts != tt.wantRestarts || *stops != tt.wantStops {
				t.Errorf("restarts, stops = %d, %d; want %d, %d", *restarts, *stops, tt.wantRestarts, tt.wantStops)
			}
			if z.WasActive != tt.wantActive {
				t.Errorf("WasActive = %v, want %v", z.WasActive, tt.wantActive)
			}
		})
	}
}
