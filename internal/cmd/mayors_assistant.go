package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/assistant"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var mayorsAssistantCmd = &cobra.Command{
	Use:     "mayors-assistant",
	Aliases: []string{"assistant", "ma"},
	GroupID: GroupAgents,
	Short:   "Manage the Mayor's Assistant (inbound triage buffer for the Mayor)",
	RunE:    requireSubcommand,
	Long: `Manage the Mayor's Assistant - the town-level inbound triage buffer.

The assistant receives Telegraph events, escalations, alerts and reports in
its own inbox (mayors-assistant/), consolidates them, and briefs the Mayor by
mail. It also runs the periodic loose-ends sweep on the Mayor's behalf.

Role shortcuts: "mayors-assistant" in mail/nudge addresses resolves to this agent.
See docs/design/mayors-assistant-role.md.`,
}

var (
	mayorsAssistantAgentOverride string
	mayorsAssistantStatusRunning bool
)

var mayorsAssistantStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Mayor's Assistant session",
	RunE:  runMayorsAssistantStart,
}

var mayorsAssistantStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Mayor's Assistant session",
	RunE:  runMayorsAssistantStop,
}

var mayorsAssistantAttachCmd = &cobra.Command{
	Use:     "attach",
	Aliases: []string{"at"},
	Short:   "Attach to the Mayor's Assistant session (starts it if needed)",
	RunE:    runMayorsAssistantAttach,
}

var mayorsAssistantStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check Mayor's Assistant session status",
	RunE:  runMayorsAssistantStatus,
}

var mayorsAssistantRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Mayor's Assistant session",
	RunE:  runMayorsAssistantRestart,
}

func init() {
	mayorsAssistantCmd.AddCommand(mayorsAssistantStartCmd)
	mayorsAssistantCmd.AddCommand(mayorsAssistantStopCmd)
	mayorsAssistantCmd.AddCommand(mayorsAssistantAttachCmd)
	mayorsAssistantCmd.AddCommand(mayorsAssistantStatusCmd)
	mayorsAssistantCmd.AddCommand(mayorsAssistantRestartCmd)

	mayorsAssistantStatusCmd.Flags().BoolVar(&mayorsAssistantStatusRunning, "running", false, "Output only true/false for running status")
	for _, c := range []*cobra.Command{mayorsAssistantStartCmd, mayorsAssistantAttachCmd, mayorsAssistantRestartCmd} {
		c.Flags().StringVar(&mayorsAssistantAgentOverride, "agent", "", "Agent alias to run the Mayor's Assistant with (overrides town default)")
	}

	rootCmd.AddCommand(mayorsAssistantCmd)
}

func getMayorsAssistantManager() (*assistant.Manager, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return assistant.NewManager(townRoot), nil
}

func runMayorsAssistantStart(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorsAssistantManager()
	if err != nil {
		return err
	}

	fmt.Println("Starting Mayor's Assistant session...")
	if err := mgr.Start(mayorsAssistantAgentOverride); err != nil {
		if errors.Is(err, assistant.ErrAlreadyRunning) {
			return fmt.Errorf("Mayor's Assistant session already running. Attach with: gt mayors-assistant attach")
		}
		return err
	}

	fmt.Printf("%s Mayor's Assistant session started. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render("gt mayors-assistant attach"))
	return nil
}

func runMayorsAssistantStop(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorsAssistantManager()
	if err != nil {
		return err
	}

	fmt.Println("Stopping Mayor's Assistant session...")
	if err := mgr.Stop(); err != nil {
		if errors.Is(err, assistant.ErrNotRunning) {
			return fmt.Errorf("Mayor's Assistant session is not running")
		}
		return err
	}

	fmt.Printf("%s Mayor's Assistant session stopped.\n", style.Bold.Render("✓"))
	return nil
}

func runMayorsAssistantAttach(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorsAssistantManager()
	if err != nil {
		return err
	}

	running, err := mgr.IsRunning()
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		fmt.Println("Mayor's Assistant session not running, starting...")
		if err := mgr.Start(mayorsAssistantAgentOverride); err != nil {
			return err
		}
	}

	// Shared attach helper (links if inside tmux, attaches if outside).
	return attachToTmuxSession(mgr.SessionName())
}

func runMayorsAssistantStatus(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorsAssistantManager()
	if err != nil {
		return err
	}

	info, err := mgr.Status()
	if mayorsAssistantStatusRunning {
		fmt.Println(err == nil)
		return nil
	}
	if err != nil {
		if errors.Is(err, assistant.ErrNotRunning) {
			fmt.Printf("%s Mayor's Assistant session is %s\n", style.Dim.Render("○"), "not running")
			fmt.Printf("\nStart with: %s\n", style.Dim.Render("gt mayors-assistant start"))
			return nil
		}
		return err
	}

	attachedStatus := "detached"
	if info.Attached {
		attachedStatus = "attached"
	}
	fmt.Printf("%s Mayor's Assistant is %s\n", style.Bold.Render("●"), style.Bold.Render("running"))
	fmt.Printf("  Status: %s\n", attachedStatus)
	fmt.Printf("  Created: %s\n", info.Created)
	fmt.Printf("\nAttach with: %s\n", style.Dim.Render("gt mayors-assistant attach"))
	return nil
}

func runMayorsAssistantRestart(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorsAssistantManager()
	if err != nil {
		return err
	}

	// Stop if running (ignore not-running error)
	if err := mgr.Stop(); err != nil && !errors.Is(err, assistant.ErrNotRunning) {
		return fmt.Errorf("stopping session: %w", err)
	}

	return runMayorsAssistantStart(cmd, args)
}
