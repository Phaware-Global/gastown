package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/assistant/sweep"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	assistantSweepJSON     bool
	assistantSweepDryRun   bool
	assistantSweepAck      string
	assistantSweepSendTo   string
	assistantSweepProtocol bool
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

or let the command do both, which is what the assistant's trigger asks for:

  gt assistant sweep --send-to mayor/

runs the ports and, when there is something new or a port did not fully
run, mails the report verbatim as "SWEEP: <n> new findings (<ports>)"; a
port that failed is named "not checked" in the subject. With nothing new
from ports that all ran clean it mails nothing. Either way it acknowledges
the run (after the mail, when one is sent) and prints a few summary lines,
never the report.

assistant.sweep.mode chooses who decides what the Mayor gets:
"deterministic" (default) uses --send-to as above; "inference" has the
assistant read the saved report and decide. --protocol prints the
procedure for the configured mode (the sweep trigger mail's body).

--json and --dry-run never write state, so a manual check cannot silently
consume findings the Mayor has not seen. Every human-authored value in the
report is fenced as EXTERNAL CONTENT with every line prefixed "> ".`,
	RunE: runAssistantSweep,
}

func init() {
	assistantSweepCmd.Flags().BoolVar(&assistantSweepJSON, "json", false, "Emit the report as JSON (does not record state)")
	assistantSweepCmd.Flags().BoolVar(&assistantSweepDryRun, "dry-run", false, "Local ports run; external ports only print the queries they would issue; nothing is recorded")
	assistantSweepCmd.Flags().StringVar(&assistantSweepAck, "ack", "", "Acknowledge a pending run (after mailing the Mayor) by its run ID")
	assistantSweepCmd.Flags().StringVar(&assistantSweepSendTo, "send-to", "", "Run, mail the report verbatim to mayor/ (the only accepted address) when there is something new, and acknowledge the run")
	assistantSweepCmd.Flags().BoolVar(&assistantSweepProtocol, "protocol", false, "Print the sweep procedure for the configured assistant.sweep.mode (the trigger mail body) and exit")
	assistantCmd.AddCommand(assistantSweepCmd)
}

func runAssistantSweep(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if assistantSweepProtocol {
		settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
		if err != nil {
			return fmt.Errorf("loading town settings: %w", err)
		}
		fmt.Print(sweep.TriggerBody(settings.SweepMode()))
		return nil
	}
	if assistantSweepSendTo != "" && (assistantSweepJSON || assistantSweepDryRun || assistantSweepAck != "") {
		return fmt.Errorf("--send-to runs a recorded sweep: it cannot be combined with --json, --dry-run or --ack")
	}
	if cmd.Flags().Changed("ack") && strings.TrimSpace(assistantSweepAck) == "" {
		return fmt.Errorf("--ack needs a run ID; an empty value (e.g. a sed that matched nothing) must not start a new recorded sweep")
	}
	if assistantSweepSendTo != "" && strings.TrimSuffix(assistantSweepSendTo, "/") != strings.TrimSuffix(sweepRecipient, "/") {
		return fmt.Errorf("--send-to only accepts %s (the sweep report is for the Mayor)", sweepRecipient)
	}
	assistantSweepAck = strings.TrimSpace(assistantSweepAck) // a CR or space left by the recipe's sed
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
	report, err := runner.Run(context.Background(), sweep.Options{Write: !assistantSweepJSON && !assistantSweepDryRun, DryRun: assistantSweepDryRun})
	if err != nil {
		return err
	}
	if assistantSweepSendTo != "" {
		return sendSweepReport(townRoot, assistantSweepSendTo, report)
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

// Seams for tests: how a sweep report is mailed and how a run is acked.
var (
	sweepSend = func(workDir string, msg *mail.Message) error {
		router := mail.NewRouter(workDir)
		defer router.WaitPendingNotifications()
		return router.Send(msg)
	}
	sweepAck = func(townRoot, runID string) error {
		return sweep.NewStore(townRoot).Ack(runID, time.Now())
	}
)

// sweepRecipient is the only --send-to address: the sweep report is for the
// Mayor, and a trigger mail (unauthenticated, as all mail is) must not be
// able to redirect it to another agent and ack it there.
const sweepRecipient = "mayor/"

// sendSweepReport mails the rendered report verbatim to the Mayor and
// acknowledges the run only after the send succeeded, so a failed send leaves
// the run pending and the next sweep reports everything again. A run with
// nothing new from ports that all ran clean is acked without a mail. The
// assistant never handles the report text: it only sees the summary.
func sendSweepReport(townRoot, to string, report *sweep.Report) error {
	if strings.TrimSuffix(to, "/") != strings.TrimSuffix(sweepRecipient, "/") {
		return fmt.Errorf("--send-to only accepts %s (the sweep report is for the Mayor); run %s is pending, not acknowledged", sweepRecipient, report.RunID)
	}
	to = sweepRecipient
	if !sweep.ShouldMail(report) {
		if report.Pending {
			if err := sweepAck(townRoot, report.RunID); err != nil {
				return fmt.Errorf("nothing new, but run %s could not be acknowledged: %w", report.RunID, err)
			}
		}
		fmt.Printf("%s Nothing new: no mail sent to %s; run %s acknowledged\n%s", style.Bold.Render("✓"), to, report.RunID, sweep.Summary(report))
		return nil
	}
	// The sender must resolve to a real agent identity: detectSender falls
	// back to "overseer", the human, when GT_ROLE is unset, unknown, or lacks
	// its rig/name variables, and this report must never claim the human.
	from := detectSender()
	if from == "" || from == "overseer" {
		return fmt.Errorf("the sender resolves to %q, not an agent identity (check GT_ROLE and its rig/name variables); run %s is pending, not acknowledged", from, report.RunID)
	}
	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	msg := mail.NewMessage(from, to, sweep.Subject(report), sweep.Markdown(report))
	msg.ThreadID = generateThreadID()
	msg.Wisp = true // like gt mail send's default: a report every interval must not pile up permanent beads
	if err := sweepSend(workDir, msg); err != nil {
		return fmt.Errorf("sending the sweep report to %s: %w (run %s is pending, not acknowledged)", to, err, report.RunID)
	}
	_ = events.LogFeed(events.TypeMail, from, events.MailPayload(to, msg.Subject))
	if report.Pending {
		if err := sweepAck(townRoot, report.RunID); err != nil {
			return fmt.Errorf("report mailed to %s but run %s could not be acknowledged: %w", to, report.RunID, err)
		}
	}
	fmt.Printf("%s Sweep report mailed to %s and run %s acknowledged\n%s", style.Bold.Render("✓"), to, report.RunID, sweep.Summary(report))
	return nil
}
