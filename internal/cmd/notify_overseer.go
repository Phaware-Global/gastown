package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// notify-overseer command flags
var (
	notifyOverseerSubject  string
	notifyOverseerMessage  string
	notifyOverseerStdin    bool
	notifyOverseerRefs     []string
	notifyOverseerCategory string
)

// notifyOverseerCategories are the only categories allowed to reach Slack.
// Product, stakeholder, and feature questions stay in Jira/GitHub.
var notifyOverseerCategories = []string{"infra", "ops", "security"}

var notifyOverseerCmd = &cobra.Command{
	Use:     "notify-overseer",
	GroupID: GroupComm,
	Short:   "Post a composed ops brief to the overseer's Slack (mayor only)",
	Long: `Post a composed, human-readable ops brief to the overseer via Slack.

This is the ONLY path to Slack. gt escalate never posts to Slack on its own
(settings/escalation.json: slack_overseer_only, default true). The mayor
batches related escalations, decides whether a human is actually blocked,
and sends one brief per incident.

Slack is for infrastructure and operational blocks only: service down, agent
unresponsive, merge queue stuck, external dependency unreachable, credential
rotation. Product direction, stakeholder asks, feature scoping, and PR review
requests must never reach Slack — route those through GitHub or Jira.

Each referenced bead is labelled slack-forwarded:<timestamp> as an audit trail.

Examples:
  gt notify-overseer -s "CI pipeline down" -m "GitHub Actions runners offline since 09:40; 4 PRs blocked." --category infra --refs hq-abc,hq-def
  gt notify-overseer -s "Disk 96% full on agent host" --category ops --refs hq-an95vu --stdin <<'BODY'
  Docker.raw holds 98 GB unused since 10/08. Polecat spawns are failing.
  Needs a human to prune/reset Docker; agents cannot do this safely.
  BODY`,
	RunE: runNotifyOverseer,
}

func init() {
	notifyOverseerCmd.Flags().StringVarP(&notifyOverseerSubject, "subject", "s", "", "Subject line (required)")
	notifyOverseerCmd.Flags().StringVarP(&notifyOverseerMessage, "message", "m", "", "Composed summary body")
	notifyOverseerCmd.Flags().BoolVar(&notifyOverseerStdin, "stdin", false, "Read the message body from stdin (avoids shell quoting issues)")
	notifyOverseerCmd.Flags().StringSliceVar(&notifyOverseerRefs, "refs", nil, "Comma-separated escalation bead IDs to attach as context")
	notifyOverseerCmd.Flags().StringVar(&notifyOverseerCategory, "category", "", "Incident category: infra, ops, or security (required)")
	_ = notifyOverseerCmd.MarkFlagRequired("subject")
	_ = notifyOverseerCmd.MarkFlagRequired("category")

	rootCmd.AddCommand(notifyOverseerCmd)
}

// validateNotifyOverseerCategory rejects anything outside the ops-only allowlist.
func validateNotifyOverseerCategory(category string) (string, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	for _, allowed := range notifyOverseerCategories {
		if category == allowed {
			return category, nil
		}
	}
	return "", fmt.Errorf("invalid --category %q: must be one of %s (product, stakeholder, and feature questions go to GitHub/Jira, not Slack)",
		category, strings.Join(notifyOverseerCategories, ", "))
}

// validateNotifyOverseerSender restricts the command to the mayor. A human at
// the terminal (no agent identity) is also allowed so the overseer can test
// the webhook; every other agent role is rejected. detectSender yields
// "mayor/" from GT_ROLE and "mayor" from the cwd fallback, so compare without
// the trailing slash.
func validateNotifyOverseerSender(sender string) error {
	switch strings.TrimSuffix(sender, "/") {
	case "mayor", "overseer":
		return nil
	}
	return fmt.Errorf("gt notify-overseer is restricted to the mayor (current identity: %s); escalate with gt escalate and let the mayor decide", sender)
}

// notifyOverseerRef is one referenced bead rendered in the brief.
type notifyOverseerRef struct {
	ID    string
	Title string
	// bd is bound to the database that owns the bead (rig-prefixed IDs
	// route to their rig), so the audit label lands next to the bead.
	bd *beads.Beads
}

// slackEscape neutralises Slack mrkdwn control sequences (<!channel>, <@U..>,
// <url|text>) in text that was not composed by the mayor, so an agent-written
// bead title cannot ping the channel or plant a disguised link in the brief.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// formatOpsBrief renders the Slack payload for an ops brief. Subject and body
// are mayor-composed and rendered as written; referenced bead IDs and titles
// are pulled from agent-written beads and therefore escaped.
func formatOpsBrief(now time.Time, category, subject, body string, refs []notifyOverseerRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚨 *Gastown Ops Brief – %s*\n", now.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "`[%s]` *%s*\n", strings.ToUpper(category), subject)
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	if len(refs) == 0 {
		return b.String()
	}
	b.WriteString("\n*References:*\n")
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		id := slackEscape(ref.ID)
		ids = append(ids, id)
		if ref.Title != "" {
			fmt.Fprintf(&b, "• %s — %s\n", id, slackEscape(ref.Title))
		} else {
			fmt.Fprintf(&b, "• %s\n", id)
		}
	}
	fmt.Fprintf(&b, "\n_Ack with: `gt escalate ack %s`_\n", strings.Join(ids, " "))
	return b.String()
}

// slackForwardedLabel is the audit-trail label added to each referenced bead.
func slackForwardedLabel(now time.Time) string {
	return "slack-forwarded:" + now.UTC().Format("20060102T150405Z")
}

func runNotifyOverseer(cmd *cobra.Command, args []string) error {
	if notifyOverseerStdin {
		if notifyOverseerMessage != "" {
			return fmt.Errorf("cannot use --stdin with --message/-m")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		notifyOverseerMessage = strings.TrimRight(string(data), "\n")
	}
	if strings.TrimSpace(notifyOverseerSubject) == "" {
		return fmt.Errorf("--subject is required")
	}
	if strings.TrimSpace(notifyOverseerMessage) == "" {
		return fmt.Errorf("a message body is required (--message/-m or --stdin)")
	}
	category, err := validateNotifyOverseerCategory(notifyOverseerCategory)
	if err != nil {
		return err
	}
	if err := validateNotifyOverseerSender(detectSender()); err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	escalationConfig, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading escalation config: %w", err)
	}
	if escalationConfig.Contacts.SlackWebhook == "" {
		return fmt.Errorf("contacts.slack_webhook not configured in settings/escalation.json")
	}

	// Resolve references before posting so a typo fails the whole send
	// rather than producing a brief that points at nothing.
	townBeadsDir := beads.ResolveBeadsDir(townRoot)
	refs := make([]notifyOverseerRef, 0, len(notifyOverseerRefs))
	for _, id := range notifyOverseerRefs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		targetDir := beads.ResolveRoutingTarget(townRoot, id, townBeadsDir)
		refBd := beads.NewWithBeadsDir(filepath.Dir(targetDir), targetDir)
		issue, err := refBd.Show(id)
		if err != nil {
			return fmt.Errorf("resolving --refs %s: %w", id, err)
		}
		refs = append(refs, notifyOverseerRef{ID: id, Title: issue.Title, bd: refBd})
	}

	now := time.Now()
	if err := postSlackWebhook(escalationConfig.Contacts.SlackWebhook, formatOpsBrief(now, category, notifyOverseerSubject, notifyOverseerMessage, refs)); err != nil {
		return fmt.Errorf("posting ops brief to slack: %w", err)
	}
	fmt.Printf("💬 Ops brief posted to Slack [%s] %s\n", strings.ToUpper(category), notifyOverseerSubject)

	label := slackForwardedLabel(now)
	for _, ref := range refs {
		if err := ref.bd.Update(ref.ID, beads.UpdateOptions{AddLabels: []string{label}}); err != nil {
			style.PrintWarning("posted, but failed to record forward on %s: %v", ref.ID, err)
			continue
		}
		fmt.Printf("  🏷  %s labelled %s\n", ref.ID, label)
	}
	return nil
}
