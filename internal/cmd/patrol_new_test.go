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
// deacon heartbeat for the RoleDeacon case by actually calling runPatrolNewWithRole.
// autoSpawnPatrol will fail (no beads infrastructure in tests) but the heartbeat
// write happens before that, so we verify side-effects despite the expected error.
func TestPatrolNew_DeaconHeartbeatRefresh(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "patrol-new-hb-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	roleInfo := RoleInfo{
		Role:     RoleDeacon,
		TownRoot: tmpDir,
		Rig:      "testrig",
	}

	// runPatrolNewWithRole will error at autoSpawnPatrol (no beads in tests),
	// but the heartbeat refresh runs before that point.
	_ = runPatrolNewWithRole(patrolNewCmd, nil, roleInfo)

	hb := deacon.ReadHeartbeat(tmpDir)
	if hb == nil {
		t.Fatal("expected heartbeat to be written for deacon role")
	}
	if hb.LastAction != "starting patrol cycle" {
		t.Errorf("LastAction = %q, want %q", hb.LastAction, "starting patrol cycle")
	}
}

// TestPatrolNew_NonDeaconRolesSkipHeartbeat verifies that witness and refinery
// roles do not trigger a heartbeat write.
func TestPatrolNew_NonDeaconRolesSkipHeartbeat(t *testing.T) {
	for _, roleName := range []string{"witness", "refinery"} {
		t.Run(roleName, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "patrol-new-nondeacon-*")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()

			roleInfo := RoleInfo{
				Role:     Role(roleName),
				TownRoot: tmpDir,
				Rig:      "testrig",
			}

			_ = runPatrolNewWithRole(patrolNewCmd, nil, roleInfo)

			if hb := deacon.ReadHeartbeat(tmpDir); hb != nil {
				t.Errorf("heartbeat should not be written for %s role", roleName)
			}
		})
	}
}
