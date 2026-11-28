package session

import (
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
)

func init() {
	tmux.SetSessionRoleResolver(secretRoleForSession)
}

// secretRoleForSession returns the GT_ROLE that config.AgentEnv gives the agent
// whose session has this name. It reports false for a name gt does not use for
// an agent.
func secretRoleForSession(name string) (string, bool) {
	identity, err := ParseSessionName(name)
	if err != nil {
		return "", false
	}
	// ParseSessionName reads a reviewer session as a polecat named "reviewer".
	if identity.Prefix != "" && name == ReviewerSessionName(identity.Prefix) {
		return identity.Rig + "/" + constants.RoleReviewer, true
	}
	switch {
	case identity.Role == RoleOverseer:
		return "", false
	case identity.Role == RoleDog:
		return "dog", true
	case identity.Role == RoleDeacon && identity.Name == "boot":
		return "deacon/boot", true
	}
	return identity.GTRole(), true
}
