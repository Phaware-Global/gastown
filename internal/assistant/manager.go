// Package assistant manages the Mayor's Assistant session lifecycle.
//
// The assistant is a town-level singleton (hq-assistant) that triages
// inbound mail for the Mayor. It mirrors the Mayor's tmux lifecycle (no ACP
// mode) and runs from <town>/assistant. See
// docs/design/assistant-role.md.
package assistant

import (
	"errors"
	"fmt"
	"github.com/steveyegge/gastown/internal/nudge"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Common errors
var (
	ErrNotRunning     = errors.New("assistant not running")
	ErrAlreadyRunning = errors.New("assistant already running")
)

// Manager handles Mayor's Assistant lifecycle operations.
type Manager struct {
	townRoot string
}

// NewManager creates a new assistant manager for a town.
func NewManager(townRoot string) *Manager {
	return &Manager{townRoot: townRoot}
}

// SessionName returns the tmux session name for the assistant.
func SessionName() string {
	return session.AssistantSessionName()
}

// SessionName returns the tmux session name for the assistant.
func (m *Manager) SessionName() string {
	return SessionName()
}

// WorkDir returns the assistant's working directory (<town>/assistant).
func (m *Manager) WorkDir() string {
	return filepath.Join(m.townRoot, constants.RoleAssistant)
}

// Start starts the assistant tmux session.
// agentOverride optionally specifies a different agent alias to use; otherwise
// role_agents["assistant"] (then the town default) is resolved by the
// shared session lifecycle.
func (m *Manager) Start(agentOverride string) error {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	// Kill any existing zombie session (tmux alive but agent dead).
	// Returns error if session is healthy and already running.
	if _, err := session.KillExistingSession(t, sessionID, true); err != nil {
		return ErrAlreadyRunning
	}

	// Refuse to start in a town whose rig registry already uses the
	// assistant's name: the work dir would be that rig's container and cwd
	// role detection, mail addresses and bead IDs would become ambiguous.
	// New rigs cannot take the name (it is reserved in rig.AddRig); a town
	// upgraded with such a rig must rename it first.
	if rigs, err := config.LoadRigsConfig(constants.MayorRigsPath(m.townRoot)); err == nil && rigs != nil {
		for name := range rigs.Rigs {
			// Case-insensitive: on a case-insensitive filesystem "Assistant"
			// and "assistant" are the same directory.
			if strings.EqualFold(name, constants.RoleAssistant) {
				return fmt.Errorf("a rig named %q is registered in rigs.json; that name is reserved for the Mayor's Assistant (work dir <town>/%s) and the rig has been excluded from town-root scans since this gt version was installed — rename the rig, then start the assistant", name, constants.RoleAssistant)
			}
		}
	}

	workDir := m.WorkDir()
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("creating assistant directory: %w", err)
	}

	// Same account resolution as the Mayor (accounts.json → CLAUDE_CONFIG_DIR).
	accountsPath := constants.MayorAccountsPath(m.townRoot)
	claudeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if claudeConfigDir == "" {
		claudeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}

	theme := tmux.ResolveSessionTheme(m.townRoot, "", constants.RoleAssistant, "")
	_, err := session.StartSession(t, session.SessionConfig{
		SessionID:        sessionID,
		WorkDir:          workDir,
		Role:             constants.RoleAssistant,
		TownRoot:         m.townRoot,
		AgentName:        "Mayor's Assistant",
		RuntimeConfigDir: claudeConfigDir,
		Beacon: session.BeaconConfig{
			Recipient: constants.RoleAssistant,
			Sender:    "human",
			Topic:     "cold-start",
		},
		AgentOverride: agentOverride,
		Theme:         theme,
		WaitForAgent:  true,
		WaitFatal:     true,
		AutoRespawn:   true,
		AcceptBypass:  true,
	})
	if err != nil {
		return err
	}

	// Start the nudge-queue poller, as the witness and refinery managers do.
	// Mail and telegraph nudges reach this session through the queue; a
	// Claude session drains it from its UserPromptSubmit hook, but an
	// opencode session has no such hook and, when idle, never submits a
	// prompt, so without the poller every queued nudge sits undelivered.
	if _, pollerErr := nudge.StartPoller(m.townRoot, sessionID); pollerErr != nil {
		log.Printf("warning: could not start nudge poller for %s: %v", sessionID, pollerErr)
	}

	time.Sleep(session.ShutdownDelay())
	return nil
}

// Stop stops the assistant session.
func (m *Manager) Stop() error {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrNotRunning
	}

	// Try graceful shutdown first (best-effort interrupt)
	_ = t.SendKeysRaw(sessionID, "C-c")
	time.Sleep(100 * time.Millisecond)

	if err := t.KillSessionWithProcesses(sessionID); err != nil {
		return fmt.Errorf("killing session: %w", err)
	}
	return nil
}

// IsRunning checks if the assistant tmux session exists.
func (m *Manager) IsRunning() (bool, error) {
	return tmux.NewTmux().HasSession(m.SessionName())
}

// Status returns information about the assistant session.
func (m *Manager) Status() (*tmux.SessionInfo, error) {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return nil, ErrNotRunning
	}
	return t.GetSessionInfo(sessionID)
}
