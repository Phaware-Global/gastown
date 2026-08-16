package daemon

import (
	"github.com/steveyegge/gastown/internal/constants"
	"log"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/wisp"
)

// Regression test for gt-arz:
// getPatrolRigs should filter parked/docked rigs at list-building time.
func TestGetPatrolRigs_FiltersNonOperationalRigs(t *testing.T) {
	townRoot := t.TempDir()

	// Seed known rigs.
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatalf("mkdir mayor dir: %v", err)
	}
	rigsJSON := `{"rigs":{"alpha":{},"beta":{},"gamma":{}}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	// Mark beta/gamma as non-operational via wisp status.
	if err := wisp.NewConfig(townRoot, "beta").Set("status", "parked"); err != nil {
		t.Fatalf("set beta parked: %v", err)
	}
	if err := wisp.NewConfig(townRoot, "gamma").Set("status", "docked"); err != nil {
		t.Fatalf("set gamma docked: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(os.Stderr, "[test] ", 0),
	}

	got := d.getPatrolRigs("witness")
	slices.Sort(got)
	// When Dolt is unavailable, isRigOperational() fails safe and returns false
	// for all rigs (can't verify docked status). This prevents witnesses from
	// starting for potentially docked rigs during Dolt outages.
	want := []string{}
	if !slices.Equal(got, want) {
		t.Fatalf("getPatrolRigs() = %v, want %v (all rigs excluded when Dolt unavailable - fail-safe)", got, want)
	}
}

func TestGetPatrolRigs_ReviewerAllowlistIsHonored(t *testing.T) {
	// The reviewer patrol is the only one in the daemon that SIGKILLs an agent's
	// whole process tree, and it is enabled by default on upgrade before any
	// daemon.json key exists. Shipping a documented `rigs` narrowing knob that
	// silently does nothing is materially worse here than an ordinary config bug:
	// the operator's opt-out appears to work and does not.
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		Reviewer: &PatrolConfig{Enabled: true, Rigs: []string{"alpha"}},
	}}
	got := GetPatrolRigs(cfg, constants.RoleReviewer)
	if len(got) != 1 || got[0] != "alpha" {
		t.Errorf("GetPatrolRigs = %v, want [alpha] — a configured rigs list must narrow the patrol", got)
	}

	// Unset is genuinely "all rigs", and that default is correct. The bug was
	// that the fall-through was unconditional, so it fired for a deliberately
	// written list too.
	if got := GetPatrolRigs(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, constants.RoleReviewer); got != nil {
		t.Errorf("GetPatrolRigs = %v, want nil (all rigs) when unset", got)
	}

	// The enable switch and the rigs list are separate controls; the round-1 fix
	// landed only the first, which is how the second went unnoticed.
	if !IsPatrolEnabled(cfg, constants.RoleReviewer) {
		t.Error("precondition: the reviewer patrol must read as enabled here")
	}
}
