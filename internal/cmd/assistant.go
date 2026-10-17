package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/assistant"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var assistantCmd = &cobra.Command{
	Use:     "assistant",
	Aliases: []string{"mayors-assistant", "ma"},
	GroupID: GroupAgents,
	Short:   "Manage the Mayor's Assistant (inbound triage buffer for the Mayor)",
	RunE:    requireSubcommand,
	Long: `Manage the Mayor's Assistant - the town-level inbound triage buffer.

The assistant receives Telegraph events, escalations, alerts and reports in
its own inbox (assistant/), consolidates them, and briefs the Mayor by
mail. It also runs the periodic loose-ends sweep on the Mayor's behalf.

Role shortcuts: "assistant" in mail/nudge addresses resolves to this agent.
See docs/design/assistant-role.md.`,
}

var (
	assistantAgentOverride string
	assistantStatusRunning bool
)

var assistantStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Mayor's Assistant session",
	RunE:  runAssistantStart,
}

var assistantStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Mayor's Assistant session",
	RunE:  runAssistantStop,
}

var assistantAttachCmd = &cobra.Command{
	Use:     "attach",
	Aliases: []string{"at"},
	Short:   "Attach to the Mayor's Assistant session (starts it if needed)",
	RunE:    runAssistantAttach,
}

var assistantStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check Mayor's Assistant session status",
	RunE:  runAssistantStatus,
}

var assistantRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Mayor's Assistant session",
	RunE:  runAssistantRestart,
}

func init() {
	assistantCmd.AddCommand(assistantStartCmd)
	assistantCmd.AddCommand(assistantStopCmd)
	assistantCmd.AddCommand(assistantAttachCmd)
	assistantCmd.AddCommand(assistantStatusCmd)
	assistantCmd.AddCommand(assistantRestartCmd)

	assistantStatusCmd.Flags().BoolVar(&assistantStatusRunning, "running", false, "Output only true/false for running status")
	for _, c := range []*cobra.Command{assistantStartCmd, assistantAttachCmd, assistantRestartCmd} {
		c.Flags().StringVar(&assistantAgentOverride, "agent", "", "Agent alias to run the Mayor's Assistant with (overrides town default)")
	}

	rootCmd.AddCommand(assistantCmd)
}

func getAssistantManager() (*assistant.Manager, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return assistant.NewManager(townRoot), nil
}

func runAssistantStart(cmd *cobra.Command, args []string) error {
	mgr, err := getAssistantManager()
	if err != nil {
		return err
	}

	fmt.Println("Starting Mayor's Assistant session...")
	if err := mgr.Start(assistantAgentOverride); err != nil {
		if errors.Is(err, assistant.ErrAlreadyRunning) {
			return fmt.Errorf("Mayor's Assistant session already running. Attach with: gt assistant attach")
		}
		return err
	}

	fmt.Printf("%s Mayor's Assistant session started. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render("gt assistant attach"))
	return nil
}

func runAssistantStop(cmd *cobra.Command, args []string) error {
	mgr, err := getAssistantManager()
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

func runAssistantAttach(cmd *cobra.Command, args []string) error {
	mgr, err := getAssistantManager()
	if err != nil {
		return err
	}

	running, err := mgr.IsRunning()
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		fmt.Println("Mayor's Assistant session not running, starting...")
		if err := mgr.Start(assistantAgentOverride); err != nil {
			return err
		}
	}

	// Shared attach helper (links if inside tmux, attaches if outside).
	return attachToTmuxSession(mgr.SessionName())
}

func runAssistantStatus(cmd *cobra.Command, args []string) error {
	mgr, err := getAssistantManager()
	if err != nil {
		return err
	}

	info, err := mgr.Status()
	if assistantStatusRunning {
		fmt.Println(err == nil)
		return nil
	}
	if err != nil {
		if errors.Is(err, assistant.ErrNotRunning) {
			fmt.Printf("%s Mayor's Assistant session is %s\n", style.Dim.Render("○"), "not running")
			fmt.Printf("\nStart with: %s\n", style.Dim.Render("gt assistant start"))
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
	fmt.Printf("\nAttach with: %s\n", style.Dim.Render("gt assistant attach"))
	return nil
}

func runAssistantRestart(cmd *cobra.Command, args []string) error {
	mgr, err := getAssistantManager()
	if err != nil {
		return err
	}

	// Stop if running (ignore not-running error)
	if err := mgr.Stop(); err != nil && !errors.Is(err, assistant.ErrNotRunning) {
		return fmt.Errorf("stopping session: %w", err)
	}

	return runAssistantStart(cmd, args)
}
