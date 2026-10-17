// Package hookutil provides shared utilities for agent hook installers.
package hookutil

import "github.com/steveyegge/gastown/internal/constants"

// IsAutonomousRole returns true if the given role operates without human
// prompting and needs automatic mail injection on startup.
//
// Autonomous roles: polecat, witness, refinery, deacon, boot, reviewer,
// mayors-assistant (unattended inbox triage; mail must arrive at SessionStart).
// Interactive roles: mayor, crew (and anything else).
//
// This is the single source of truth for the autonomous/interactive
// classification used by all hook installer packages (claude, gemini,
// cursor, etc.) and the runtime fallback logic.
func IsAutonomousRole(role string) bool {
	switch role {
	case constants.RolePolecat, constants.RoleWitness, constants.RoleRefinery, constants.RoleDeacon, constants.RoleReviewer, constants.RoleMayorsAssistant, "boot":
		return true
	default:
		return false
	}
}
