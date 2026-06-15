package reviewer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Manager handles the lifecycle of a rig's on-demand Reviewer session.
//
// It mirrors the Refinery manager's ZFC design: there is no state file — the
// tmux session is the source of truth, and review-request work is carried on
// beads/mail. The Reviewer is spawn-on-demand (one session per rig, drained by
// mail), so the Manager only needs to start, stop, and report on the session,
// and to provision the reviewer worktree if it is missing.
type Manager struct {
	rig    *rig.Rig
	output io.Writer
}

// Common errors.
var (
	ErrNotRunning     = errors.New("reviewer not running")
	ErrAlreadyRunning = errors.New("reviewer already running")
)

// NewManager creates a Reviewer manager for a rig.
func NewManager(r *rig.Rig) *Manager {
	return &Manager{rig: r, output: os.Stdout}
}

// SetOutput overrides the user-facing output writer (for tests).
func (m *Manager) SetOutput(w io.Writer) { m.output = w }

// SessionName returns the tmux session name for this rig's Reviewer.
func (m *Manager) SessionName() string {
	return session.ReviewerSessionName(session.PrefixFor(m.rig.Name))
}

// rigDir returns the reviewer worktree path (<rig>/reviewer/rig).
func (m *Manager) rigDir() string {
	return filepath.Join(m.rig.Path, "reviewer", "rig")
}

// IsRunning reports whether the Reviewer session is alive and healthy (tmux
// session present AND agent process alive — not a zombie).
func (m *Manager) IsRunning() (bool, error) {
	t := tmux.NewTmux()
	return t.CheckSessionHealth(m.SessionName(), 0) == tmux.SessionHealthy, nil
}

// Status returns information about the Reviewer session.
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

// Stop terminates the Reviewer session.
func (m *Manager) Stop() error {
	t := tmux.NewTmux()
	sessionID := m.SessionName()
	if running, _ := t.HasSession(sessionID); !running {
		return ErrNotRunning
	}
	return t.KillSession(sessionID)
}

// EnsureRunning starts the Reviewer session if it isn't already running.
// Returns nil (no error) when a healthy session already exists, so callers can
// dispatch idempotently: a second review request for the same rig simply
// queues in the running session's mailbox. extraEnv is applied only when a new
// session is started (an already-running session keeps its original env).
func (m *Manager) EnsureRunning(agentOverride string, extraEnv map[string]string) error {
	if running, _ := m.IsRunning(); running {
		return nil
	}
	return m.Start(agentOverride, extraEnv)
}

// Start spawns the Reviewer agent in a tmux session. ZFC-compliant: no state
// file. The reviewer worktree is auto-provisioned from the shared bare repo if
// missing, mirroring the Refinery's self-healing worktree behavior. extraEnv
// (e.g. GH_TOKEN resolved from reviewer_token_env at dispatch) is injected into
// the session via tmux -e so the agent and its gh subprocesses inherit it.
func (m *Manager) Start(agentOverride string, extraEnv map[string]string) error {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	if running, _ := t.HasSession(sessionID); running {
		if t.IsAgentAlive(sessionID) {
			return ErrAlreadyRunning
		}
		// Zombie — tmux alive but agent dead. Kill and recreate.
		_, _ = fmt.Fprintln(m.output, "⚠ Detected zombie reviewer session (tmux alive, agent dead). Recreating...")
		if err := t.KillSession(sessionID); err != nil {
			return fmt.Errorf("killing zombie session: %w", err)
		}
	}

	reviewerRigDir, err := m.ensureWorktree()
	if err != nil {
		return fmt.Errorf("provisioning reviewer worktree: %w", err)
	}

	townRoot := filepath.Dir(m.rig.Path)

	accountsPath := constants.MayorAccountsPath(townRoot)
	runtimeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if runtimeConfigDir == "" {
		runtimeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}

	runtimeConfig := config.ResolveRoleAgentConfig("reviewer", townRoot, m.rig.Path)
	reviewerSettingsDir := config.RoleSettingsDir("reviewer", m.rig.Path)
	if err := runtime.EnsureSettingsForRole(reviewerSettingsDir, reviewerRigDir, "reviewer", runtimeConfig); err != nil {
		return fmt.Errorf("ensuring runtime settings: %w", err)
	}
	if err := rig.EnsureGitignorePatterns(reviewerRigDir); err != nil {
		style.PrintWarning("could not update reviewer .gitignore: %v", err)
	}

	initialPrompt := session.BuildStartupPrompt(session.BeaconConfig{
		Recipient: session.BeaconRecipient("reviewer", "", m.rig.Name),
		Sender:    "refinery",
		Topic:     "review",
	}, "Run `gt prime --hook` and check your hook/mail for a review request.")

	command, err := config.BuildStartupCommandFromConfig(config.AgentEnvConfig{
		Role:             "reviewer",
		Rig:              m.rig.Name,
		TownRoot:         townRoot,
		RuntimeConfigDir: runtimeConfigDir,
		Prompt:           initialPrompt,
		Topic:            "review",
		SessionName:      sessionID,
	}, m.rig.Path, initialPrompt, agentOverride)
	if err != nil {
		return fmt.Errorf("building startup command: %w", err)
	}

	envVars := config.AgentEnv(config.AgentEnvConfig{
		Role:             "reviewer",
		Rig:              m.rig.Name,
		TownRoot:         townRoot,
		RuntimeConfigDir: runtimeConfigDir,
		Agent:            agentOverride,
		SessionName:      sessionID,
	})
	envVars = session.MergeRuntimeLivenessEnv(envVars, runtimeConfig)
	envVars["GT_REVIEWER"] = "1"
	runID := uuid.New().String()
	envVars["GT_RUN"] = runID
	// Inject dispatch-resolved secrets (e.g. GH_TOKEN) last so they win.
	for k, v := range extraEnv {
		if v != "" {
			envVars[k] = v
		}
	}

	if err := t.NewSessionWithCommandAndEnv(sessionID, reviewerRigDir, command, envVars); err != nil {
		return fmt.Errorf("creating tmux session: %w", err)
	}

	theme := tmux.ResolveSessionTheme(townRoot, m.rig.Name, "reviewer", "")
	_ = t.ConfigureGasTownSession(sessionID, theme, m.rig.Name, "reviewer", "reviewer")
	_ = t.AcceptStartupDialogs(sessionID)

	if err := t.WaitForRuntimeReady(sessionID, runtimeConfig, constants.ClaudeStartTimeout); err != nil {
		_ = t.KillSessionWithProcesses(sessionID)
		return fmt.Errorf("waiting for reviewer to start: %w", err)
	}

	if _, pollerErr := nudge.StartPoller(townRoot, sessionID); pollerErr != nil {
		log.Printf("warning: could not start nudge poller for %s: %v", sessionID, pollerErr)
	}
	_ = runtime.RunStartupFallback(t, sessionID, "reviewer", runtimeConfig)
	_ = runtime.DeliverStartupPromptFallback(t, sessionID, initialPrompt, runtimeConfig, constants.ClaudeStartTimeout)
	if err := session.TrackSessionPID(townRoot, sessionID, t); err != nil {
		log.Printf("warning: tracking session PID for %s: %v", sessionID, err)
	}

	session.RecordAgentInstantiateFromDir(context.Background(), runID, runtimeConfig.ResolvedAgent,
		"reviewer", "reviewer", sessionID, m.rig.Name, townRoot, "", reviewerRigDir)
	return nil
}

// ensureWorktree returns the reviewer worktree path, provisioning a dedicated
// <rig>/reviewer/rig worktree if it does not yet exist.
//
// The Reviewer does DESTRUCTIVE detached checkouts (`gt reviewer checkout`), so
// it must NEVER share another agent's working tree — running its checkout in
// the refinery's or mayor's worktree would mutate their HEAD under their feet
// and would also fail the requireReviewerWorktree guard. So instead of falling
// back to a shared worktree, this always adds its OWN linked worktree off the
// shared object store: from the bare repo (.repo.git) when present, else from
// an existing worktree (mayor/rig or refinery/rig) used only as the source repo
// to run `git worktree add` against — the resulting reviewer worktree is fully
// isolated.
func (m *Manager) ensureWorktree() (string, error) {
	reviewerRigDir := m.rigDir()
	if _, err := os.Stat(reviewerRigDir); err == nil {
		return reviewerRigDir, nil
	}

	sourceDir := ""
	for _, cand := range []string{
		filepath.Join(m.rig.Path, ".repo.git"),
		filepath.Join(m.rig.Path, "mayor", "rig"),
		filepath.Join(m.rig.Path, "refinery", "rig"),
	} {
		if _, err := os.Stat(cand); err == nil {
			sourceDir = cand
			break
		}
	}
	if sourceDir == "" {
		return "", fmt.Errorf("no source repo to provision a reviewer worktree "+
			"(looked for %s/.repo.git, mayor/rig, refinery/rig)", m.rig.Path)
	}
	if err := m.provisionWorktree(reviewerRigDir, sourceDir); err != nil {
		return "", err
	}
	return reviewerRigDir, nil
}

// provisionWorktree adds a new, isolated reviewer/rig worktree off sourceDir
// (a bare .repo.git or an existing worktree). The reviewer worktree shares the
// object store but has its own working tree and HEAD, so detached checkouts
// there never disturb the source.
func (m *Manager) provisionWorktree(reviewerRigDir, sourceDir string) error {
	if err := os.MkdirAll(filepath.Dir(reviewerRigDir), 0o755); err != nil {
		return fmt.Errorf("creating reviewer dir: %w", err)
	}
	var srcGit *git.Git
	if strings.HasSuffix(sourceDir, ".repo.git") {
		srcGit = git.NewGitWithDir(sourceDir, "")
	} else {
		srcGit = git.NewGit(sourceDir)
	}
	_ = srcGit.WorktreePrune()
	if err := srcGit.WorktreeAddExisting(reviewerRigDir, m.rig.DefaultBranch()); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	if err := git.NewGit(reviewerRigDir).ConfigureHooksPath(); err != nil {
		_, _ = fmt.Fprintf(m.output, "⚠ Could not configure hooks for reviewer worktree: %v\n", err)
	}
	_, _ = fmt.Fprintf(m.output, "✓ Provisioned reviewer worktree at %s\n", reviewerRigDir)
	return nil
}
