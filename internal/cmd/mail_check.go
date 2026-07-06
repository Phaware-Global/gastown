package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// injectAckBudget bounds how long the inbox read may take before the inject
// hook skips the delivery-ack. The ack writes to Dolt and scales with the
// unread count; under load a slow read plus a slow ack can blow the Claude Code
// hook timeout (30s by default), discarding the hook's whole output. Skipping
// the ack when the read alone is already slow keeps the hook fast — unacked
// deliveries are simply retried on a later turn.
const injectAckBudget = 8 * time.Second

func runMailCheck(cmd *cobra.Command, args []string) error {
	start := time.Now()

	// Determine which inbox (priority: --identity flag, auto-detect)
	address := ""
	if mailCheckIdentity != "" {
		address = mailCheckIdentity
	} else {
		address = detectSender()
	}

	// All mail uses town beads (two-level architecture)
	workDir, err := findMailWorkDir()
	if err != nil {
		if mailCheckInject {
			fmt.Fprintf(os.Stderr, "gt mail check: workspace lookup failed: %v\n", err)
			return nil
		}
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get mailbox
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(address)
	if err != nil {
		if mailCheckInject {
			fmt.Fprintf(os.Stderr, "gt mail check: mailbox error for %s: %v\n", address, err)
			return nil
		}
		return fmt.Errorf("getting mailbox: %w", err)
	}

	// Inject mode handles the nudge queue FIRST, before the (slow) inbox read
	// below. The inbox read is a Dolt scan that can take seconds under load, and
	// Claude Code discards a UserPromptSubmit hook's ENTIRE stdout if the hook
	// exceeds its timeout (30s by default). Draining nudges destructively at the
	// end of that read — as the code used to — meant a slow read could push the
	// hook over the timeout after the nudges had already been removed from the
	// queue, silently losing them. Instead we CLAIM the nudges here (emitting
	// them immediately) and defer their removal to CommitClaims after the mail
	// work; a timed-out (killed) hook never reaches the commit, so the claims
	// survive on disk for the orphan sweep to redeliver. See nudge.DrainClaim.
	//
	// Guarded by !mailCheckJSON so the pure --json path (which returns early
	// below) never claims nudges it won't emit.
	var nudgeClaims []string
	if mailCheckInject && !mailCheckJSON {
		// Agent-side E-stop check (defense-in-depth). Cheap local file checks,
		// so it runs before the slow inbox read. If an E-stop is active
		// (town-wide or per-rig), inject a system reminder telling the agent to
		// checkpoint and wait. This catches agents that survived the SIGTSTP freeze.
		if townRoot, twErr := workspace.FindFromCwd(); twErr == nil {
			rigName := os.Getenv("GT_RIG")
			if estop.IsActive(townRoot) || (rigName != "" && estop.IsRigActive(townRoot, rigName)) {
				fmt.Print("<system-reminder>\n")
				fmt.Print("EMERGENCY STOP ACTIVE. All work is paused.\n")
				fmt.Print("Do NOT start new tasks or tool calls. Checkpoint your current state\n")
				fmt.Print("(save progress notes) and wait for the overseer to run 'gt thaw'.\n")
				fmt.Print("This is a system-level pause — it may be due to infrastructure failure,\n")
				fmt.Print("maintenance, or the operator traveling.\n")
				fmt.Print("</system-reminder>\n")
			}
		}

		// Claim + emit queued nudges (from --mode=queue or --mode=wait-idle
		// fallback). The nudge queue is per-session; CurrentSessionName reads
		// GT_SESSION (no tmux subprocess, which busy-spins under load).
		sessionName := tmux.CurrentSessionName()
		if sessionName != "" {
			queuedNudges, claims, drainErr := nudge.DrainClaim(workDir, sessionName)
			nudgeClaims = claims
			if drainErr != nil {
				fmt.Fprintf(os.Stderr, "gt mail check: nudge queue drain error: %v\n", drainErr)
			} else if len(queuedNudges) > 0 {
				fmt.Print(nudge.FormatForInjection(queuedNudges))
			}
		}
	}

	// Load the inbox once. The inject path needs unread messages later, and
	// calling Count() followed by ListUnread() doubles bd/Dolt reads.
	messages, _, unread, err := loadInboxSnapshot(mailbox, false)
	if err != nil {
		if mailCheckInject {
			fmt.Fprintf(os.Stderr, "gt mail check: inbox load error for %s: %v\n", address, err)
			// Clean exit: nudge output above was accepted, so finalize the claims.
			_ = nudge.CommitClaims(nudgeClaims)
			return nil
		}
		return fmt.Errorf("loading inbox: %w", err)
	}

	// JSON output
	if mailCheckJSON {
		result := map[string]interface{}{
			"address": address,
			"unread":  unread,
			"has_new": unread > 0,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	// Inject mode: notify agent of mail with priority-appropriate framing.
	// Three tiers: urgent interrupts immediately, high-priority is processed
	// at the next task boundary, normal/low is informational but still
	// checked before going idle (prevents mail from sitting unread).
	if mailCheckInject {
		if unread > 0 {
			messages = filterUnreadMessages(messages)
			fmt.Print(formatInjectOutput(messages))
			// Ack after output so message is delivered before being marked acked.
			// Skip it when the inbox read already blew the budget, so a slow read
			// under load doesn't compound into a slow write and blow the hook
			// timeout — unacked deliveries retry on a later turn.
			if time.Since(start) < injectAckBudget {
				if ackErr := mailbox.AcknowledgeDeliveries(address, messages); ackErr != nil {
					fmt.Fprintf(os.Stderr, "gt mail check: delivery ack update failed for %s: %v\n", address, ackErr)
				}
			} else {
				fmt.Fprintf(os.Stderr, "gt mail check: skipped delivery ack for %s (inbox read exceeded %s budget)\n", address, injectAckBudget)
			}
		}

		// Commit nudge removal LAST. A hook killed at the timeout never reaches
		// here, leaving the claims for the orphan sweep to redeliver.
		_ = nudge.CommitClaims(nudgeClaims)
		return nil
	}

	// Normal mode
	if unread > 0 {
		fmt.Printf("%s %d unread message(s)\n", style.Bold.Render("📬"), unread)
		return NewSilentExit(0)
	}
	fmt.Println("No new mail")
	return NewSilentExit(1)
}

// formatInjectOutput builds the system-reminder text for inject mode.
// It separates messages into three tiers (urgent, high, normal/low) and
// formats them with priority-appropriate framing for the agent.
func formatInjectOutput(messages []*mail.Message) string {
	var urgent, high, normal []*mail.Message
	for _, msg := range messages {
		switch msg.Priority {
		case mail.PriorityUrgent:
			urgent = append(urgent, msg)
		case mail.PriorityHigh:
			high = append(high, msg)
		default:
			normal = append(normal, msg)
		}
	}

	var b strings.Builder

	if len(urgent) > 0 {
		// Urgent mail: interrupt — agent should stop and read.
		b.WriteString("<system-reminder>\n")
		fmt.Fprintf(&b, "URGENT: %d urgent message(s) require immediate attention.\n\n", len(urgent))
		for _, msg := range urgent {
			fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
		}
		// Show high-priority messages separately so their "process before idle"
		// framing is preserved even when urgent messages are present.
		if len(high) > 0 {
			fmt.Fprintf(&b, "\nAlso %d high-priority message(s) — process before going idle:\n", len(high))
			for _, msg := range high {
				fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
			}
		}
		if len(normal) > 0 {
			fmt.Fprintf(&b, "\n(Plus %d additional message(s) — check after current task.)\n", len(normal))
		}
		b.WriteString("\nRun 'gt mail read <id>' to read urgent messages.\n")
		b.WriteString("</system-reminder>\n")
	} else if len(high) > 0 {
		// High-priority mail: don't interrupt, but process promptly at task boundary.
		b.WriteString("<system-reminder>\n")
		fmt.Fprintf(&b, "You have %d high-priority message(s) in your inbox.\n\n", len(high))
		for _, msg := range high {
			fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
		}
		if len(normal) > 0 {
			fmt.Fprintf(&b, "\n(Plus %d additional message(s).)\n", len(normal))
		}
		b.WriteString("\nContinue your current task. When it completes, process these messages\n")
		b.WriteString("before going idle: 'gt mail inbox'\n")
		b.WriteString("</system-reminder>\n")
	} else {
		// Normal/low mail: informational, process at next task boundary.
		b.WriteString("<system-reminder>\n")
		fmt.Fprintf(&b, "You have %d unread message(s) in your inbox.\n\n", len(normal))
		for _, msg := range normal {
			fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
		}
		b.WriteString("\nContinue your current task. When it completes, check these messages\n")
		b.WriteString("before going idle: 'gt mail inbox'\n")
		b.WriteString("</system-reminder>\n")
	}

	return b.String()
}
