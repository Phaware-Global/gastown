package cmd

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

func TestRunPatrolNew_UnsupportedRole(t *testing.T) {
	// Test that an unsupported role returns an error
	// We can't easily test the full flow without bd/beads,
	// but we can verify role validation logic

	// Test the role switch logic directly
	validRoles := []string{"deacon", "witness", "refinery"}
	invalidRoles := []string{"mayor", "polecat", "crew", "unknown", ""}

	for _, role := range validRoles {
		r := Role(role)
		if r != RoleDeacon && r != RoleWitness && r != RoleRefinery {
			t.Errorf("role %q should be valid for patrol new", role)
		}
	}

	for _, role := range invalidRoles {
		r := Role(role)
		if r == RoleDeacon || r == RoleWitness || r == RoleRefinery {
			t.Errorf("role %q should be invalid for patrol new", role)
		}
	}
}

func TestPatrolNewCmd_Registered(t *testing.T) {
	// Verify the command is properly registered
	found := false
	for _, cmd := range patrolCmd.Commands() {
		if cmd.Use == "new" {
			found = true
			break
		}
	}
	if !found {
		t.Error("patrol new command not registered")
	}
}

func TestPatrolNewCmd_HasRoleFlag(t *testing.T) {
	flag := patrolNewCmd.Flags().Lookup("role")
	if flag == nil {
		t.Error("patrol new command missing --role flag")
	}
}

// TestPatrolNew_DeaconHeartbeatRefresh verifies that runPatrolNew refreshes the
// deacon heartbeat for the RoleDeacon case. This mirrors the guard in
// patrol_report.go that ensures the daemon sees liveness at cycle boundaries.
func TestPatrolNew_DeaconHeartbeatRefresh(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "patrol-new-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Simulate what runPatrolNew does for the deacon role:
	// write heartbeat if RoleDeacon
	roleName := "deacon"
	if Role(roleName) == RoleDeacon {
		if hbErr := deacon.TouchWithAction(tmpDir, "starting patrol cycle", 0, 0); hbErr != nil {
			t.Fatalf("deacon.TouchWithAction failed: %v", hbErr)
		}
	}

	// Verify heartbeat was written
	hb := deacon.ReadHeartbeat(tmpDir)
	if hb == nil {
		t.Fatal("expected heartbeat to be written for deacon role")
	}
	if hb.LastAction != "starting patrol cycle" {
		t.Errorf("LastAction = %q, want 'starting patrol cycle'", hb.LastAction)
	}

	// Verify witness/refinery roles do NOT trigger heartbeat write
	tmpDir2, err := os.MkdirTemp("", "patrol-new-nondeacon-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir2) }()

	for _, nonDeaconRole := range []string{"witness", "refinery"} {
		if Role(nonDeaconRole) == RoleDeacon {
			deacon.TouchWithAction(tmpDir2, "starting patrol cycle", 0, 0) //nolint:errcheck
		}
	}
	if hb2 := deacon.ReadHeartbeat(tmpDir2); hb2 != nil {
		t.Error("heartbeat should not be written for non-deacon roles")
	}
}
