package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/assistant/sweep"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	assistantSweepJSON   bool
	assistantSweepDryRun bool
	assistantSweepAck    string
)

var assistantSweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Run the loose-ends sweep ports and print a report for the Mayor's Assistant",
	Long: `Run every enabled sweep port (settings/config.json → assistant.sweep.ports,
default: beads) and print the findings grouped by port.

Findings are deduped against the last ACKNOWLEDGED run: new findings are
printed in full, unchanged ones as counts. A plain run records a pending
state file; after the SWEEP: mail has been sent to mayor/, commit it with

  gt assistant sweep --ack <run-id>

--json and --dry-run never write state, so a manual check cannot silently
consume findings the Mayor has not seen. Every human-authored value in the
report is fenced as EXTERNAL CONTENT with every line prefixed "> ".`,
	RunE: runAssistantSweep,
}

func init() {
	assistantSweepCmd.Flags().BoolVar(&assistantSweepJSON, "json", false, "Emit the report as JSON (does not record state)")
	assistantSweepCmd.Flags().BoolVar(&assistantSweepDryRun, "dry-run", false, "Run the ports but do not record state")
	assistantSweepCmd.Flags().StringVar(&assistantSweepAck, "ack", "", "Acknowledge a pending run (after mailing the Mayor) by its run ID")
	assistantCmd.AddCommand(assistantSweepCmd)
}

func runAssistantSweep(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if assistantSweepAck != "" {
		if err := sweep.NewStore(townRoot).Ack(assistantSweepAck, time.Now()); err != nil {
			return err
		}
		fmt.Printf("%s Sweep run %s acknowledged\n", style.Bold.Render("✓"), assistantSweepAck)
		return nil
	}
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}
	runner := sweep.NewRunner(townRoot, settings)
	report, err := runner.Run(context.Background(), sweep.Options{Write: !assistantSweepJSON && !assistantSweepDryRun})
	if err != nil {
		return err
	}
	if assistantSweepJSON {
		data, err := sweep.JSON(report)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	fmt.Print(sweep.Markdown(report))
	return nil
}
