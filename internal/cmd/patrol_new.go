package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/style"
)

var patrolNewRole string

var patrolNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new patrol wisp with config variables",
	Long: `Create a new patrol wisp for the current role, injecting rig config
variables so the formula has correct settings baked in.

Role is auto-detected from GT_ROLE (set by the daemon). Use --role to override.

For refinery patrols, MQ config variables (run_tests, test_command,
target_branch, etc.) are read from the rig's config.json and settings/config.json and
passed as --var args to the wisp.

Examples:
  gt patrol new                  # Auto-detect role, create patrol
  gt patrol new --role refinery  # Explicitly create refinery patrol`,
	RunE: runPatrolNew,
}

func init() {
	patrolNewCmd.Flags().StringVar(&patrolNewRole, "role", "", "Role override (deacon, witness, refinery)")
}

func runPatrolNew(cmd *cobra.Command, args []string) error {
	roleInfo, err := GetRole()
	if err != nil {
		return fmt.Errorf("detecting role: %w", err)
	}
	return runPatrolNewWithRole(cmd, args, roleInfo)
}

// runPatrolNewWithRole is the testable inner implementation; callers substitute
// a controlled RoleInfo in tests to avoid touching the real workspace.
func runPatrolNewWithRole(cmd *cobra.Command, args []string, roleInfo RoleInfo) error {
	// Allow --role flag to override; otherwise use the already-parsed role
	// (GetRole already handles GT_ROLE env var internally)
	roleName := string(roleInfo.Role)
	if patrolNewRole != "" {
		roleName = patrolNewRole
	}

	// Build config based on role
	var cfg PatrolConfig
	switch Role(roleName) {
	case RoleDeacon:
		cfg = PatrolConfig{
			RoleName:      "deacon",
			PatrolMolName: constants.MolDeaconPatrol,
			BeadsDir:      roleInfo.TownRoot,
			Assignee:      "deacon",
		}
	case RoleWitness:
		cfg = PatrolConfig{
			RoleName:      "witness",
			PatrolMolName: constants.MolWitnessPatrol,
			BeadsDir:      roleInfo.Home,
			Assignee:      roleInfo.Rig + "/witness",
		}
	case RoleRefinery:
		cfg = PatrolConfig{
			RoleName:      "refinery",
			PatrolMolName: constants.MolRefineryPatrol,
			BeadsDir:      roleInfo.Home,
			Assignee:      roleInfo.Rig + "/refinery",
			ExtraVars:     buildRefineryPatrolVars(roleInfo),
		}
	default:
		return fmt.Errorf("unsupported role for patrol: %q (expected deacon, witness, or refinery)", roleName)
	}

	// For deacon: mechanically refresh heartbeat before creating the patrol cycle.
	// Covers the cold-start case where the LLM may not have run `gt deacon heartbeat`
	// before calling `gt patrol new`. The daemon kills the Deacon when heartbeat.json
	// is >20 minutes old, so touching it here ensures liveness is signaled regardless
	// of whether the formula's heartbeat step ran first.
	if Role(roleName) == RoleDeacon && roleInfo.TownRoot != "" {
		if hbErr := deacon.TouchWithAction(roleInfo.TownRoot, "starting patrol cycle", 0, 0); hbErr != nil {
			style.PrintWarning("heartbeat refresh failed: %v", hbErr)
		}
	}

	// Create and hook the wisp
	patrolID, err := autoSpawnPatrol(cfg)
	if err != nil {
		if patrolID != "" {
			// Created but failed to hook
			fmt.Fprintf(os.Stderr, "warning: %s\n", err.Error())
			fmt.Println(patrolID)
			return nil
		}
		return err
	}

	fmt.Println(patrolID)
	return nil
}
