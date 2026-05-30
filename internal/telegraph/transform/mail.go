// Package transform implements Telegraph L3: envelope builder and rate-limited nudge.
// It is provider-agnostic; it consumes NormalizedEvent from L2 and produces
// Mayor-addressed mail plus at most one nudge per configured window.
package transform

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/prompts"
	"github.com/steveyegge/gastown/internal/telegraph/tlog"
)

// MailSender abstracts mail delivery. Satisfied by *mail.Router in production
// and *mail.MemoryRouter in tests.
type MailSender interface {
	Send(msg *mail.Message) error
}

// Nudger abstracts nudge delivery so the rate-limit logic can be tested
// without spawning a subprocess.
type Nudger interface {
	Nudge(target, message string) error
}

// ExecNudger runs "gt nudge <target> --mode=queue -m <message>" as a subprocess.
// It is the production Nudger.
type ExecNudger struct{}

// Nudge delivers a nudge by running gt nudge in queue mode.
func (n *ExecNudger) Nudge(target, message string) error {
	return exec.Command("gt", nudgeCommandArgs(target, message)...).Run()
}

// nudgeCommandArgs builds the `gt nudge` argument vector for telegraph wakeups.
//
// Queue mode (not the default wait-idle): telegraph fires one wakeup per inbound
// webhook event, and its only target — the mayor — is a Claude agent with a
// UserPromptSubmit turn-boundary drain. Wait-idle would optimistically
// send-keys-inject each wakeup the instant the mayor looked idle; under the
// mayor's high event volume those injections pile into its input box during
// busy windows. Cooperative queueing drains at the next turn boundary instead.
// The mayor is still woken promptly: each underlying message also arrives as a
// per-message mail notification delivered by the router.
func nudgeCommandArgs(target, message string) []string {
	return []string{"nudge", target, "--mode=queue", "-m", message}
}

// Transformer is the L3 layer. It is safe for concurrent use.
type Transformer struct {
	sender      MailSender
	nudger      Nudger
	bodyCap     int
	nudgeWindow time.Duration
	resolver    *prompts.Resolver // nil disables operator prompt blocks
	log         *tlog.Logger      // nil disables logging

	mu        sync.Mutex
	lastNudge time.Time
}

// New returns a Transformer. bodyCap is the maximum bytes of external text
// written into the mail body; content beyond the cap is truncated. nudgeWindow
// is the minimum interval between consecutive Mayor nudges; zero disables nudges.
// resolver may be nil to disable operator prompt blocks. logger may be nil to
// disable structured logging.
func New(sender MailSender, nudger Nudger, bodyCap int, nudgeWindow time.Duration, resolver *prompts.Resolver, logger ...*tlog.Logger) *Transformer {
	var l *tlog.Logger
	if len(logger) > 0 {
		l = logger[0]
	}
	return &Transformer{
		sender:      sender,
		nudger:      nudger,
		bodyCap:     bodyCap,
		nudgeWindow: nudgeWindow,
		resolver:    resolver,
		log:         l,
	}
}

// NewProduction returns a Transformer wired to a production mail router and an
// exec-based nudger. townRoot is used to construct the Router's working directory.
// resolver may be nil to disable operator prompt blocks. logger may be nil to
// disable structured logging.
func NewProduction(townRoot string, bodyCap int, nudgeWindow time.Duration, resolver *prompts.Resolver, logger *tlog.Logger) *Transformer {
	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	return New(router, &ExecNudger{}, bodyCap, nudgeWindow, resolver, logger)
}

// Transform builds a Mayor-addressed mail envelope from event, sends it via the
// configured MailSender, and conditionally nudges Mayor within the rate-limit
// window. It is safe to call concurrently.
func (t *Transformer) Transform(event *telegraph.NormalizedEvent) error {
	var promptText, promptKey string
	if t.resolver != nil {
		promptText, promptKey = t.resolver.Resolve(event)
	}

	msg := mail.NewMessage(
		buildFrom(event),
		"mayor/",
		buildSubject(event),
		t.buildBody(event, promptText),
	)

	if err := t.sender.Send(msg); err != nil {
		return fmt.Errorf("telegraph/transform: mail send: %w", err)
	}

	// mail_id is not returned by MailSender.Send in v1; omit from log.
	t.log.Deliver(event.Provider, event.EventType, event.EventID, event.Actor, event.Subject, "", promptKey)

	t.maybeNudge(event)
	return nil
}

// maybeNudge sends a nudge to Mayor if the rate-limit window has elapsed.
// event is used for logging context only.
func (t *Transformer) maybeNudge(_ *telegraph.NormalizedEvent) {
	if t.nudgeWindow <= 0 || t.nudger == nil {
		return
	}

	t.mu.Lock()
	now := time.Now()
	if now.Sub(t.lastNudge) < t.nudgeWindow {
		t.mu.Unlock()
		t.log.NudgeSuppressed()
		return
	}
	t.lastNudge = now
	t.mu.Unlock()

	// Nudge failure is non-fatal: Mayor will discover mail on next inbox check.
	_ = t.nudger.Nudge("mayor/", "Telegraph: new events pending")
	t.log.NudgeSent()
}

// buildFrom returns the sender address: telegraph/<provider>/<actor>.
func buildFrom(event *telegraph.NormalizedEvent) string {
	return "telegraph/" + event.Provider + "/" + event.Actor
}

// buildSubject constructs the mail subject from structured NormalizedEvent fields.
// No raw user text is included; the subject is safe to display untrusted input.
//
// Format: [<PROVIDER> <SUBJECT>] <EventType prose>
func buildSubject(event *telegraph.NormalizedEvent) string {
	provider := strings.ToUpper(event.Provider)
	tag := fmt.Sprintf("[%s %s]", provider, event.Subject)

	switch event.EventType {
	case "issue.created":
		return fmt.Sprintf("%s Issue created: %s", tag, safeTitle(event.Text))
	case "issue.updated":
		return fmt.Sprintf("%s Issue updated", tag)
	case "comment.added":
		return fmt.Sprintf("%s Comment added by %s", tag, event.Actor)
	case "comment.updated":
		return fmt.Sprintf("%s Comment updated by %s", tag, event.Actor)
	case "pull_request.opened":
		return fmt.Sprintf("%s PR opened: %s", tag, safeTitle(event.Text))
	case "pull_request.merged":
		return fmt.Sprintf("%s PR merged: %s", tag, safeTitle(event.Text))
	case "pull_request.closed_unmerged":
		return fmt.Sprintf("%s PR closed (not merged): %s", tag, safeTitle(event.Text))
	case "pull_request.review_submitted":
		return fmt.Sprintf("%s Review %s by %s", tag, reviewState(event.Labels), event.Actor)
	case "pull_request.review_comment":
		return fmt.Sprintf("%s Review comment by %s", tag, event.Actor)
	case "pull_request.comment":
		return fmt.Sprintf("%s Comment added by %s", tag, event.Actor)
	case "check_run.failed":
		return fmt.Sprintf("%s Check run failed", tag)
	case "check_suite.failed":
		return fmt.Sprintf("%s Check suite failed", tag)
	case "workflow_run.failed":
		return fmt.Sprintf("%s Workflow run failed", tag)
	default:
		return fmt.Sprintf("%s %s", tag, event.EventType)
	}
}

// reviewState extracts the review state from a NormalizedEvent.Labels slice.
// The GitHub translator emits the state as "review:<state>" so the subject
// builder can surface it without poking provider payloads.
func reviewState(labels []string) string {
	const prefix = "review:"
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return "submitted"
}

// safeTitle returns at most the first line of text (no raw body content in subject).
// Truncation is rune-based to avoid splitting multi-byte UTF-8 sequences.
func safeTitle(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	const maxTitleRunes = 80
	if utf8.RuneCountInString(text) > maxTitleRunes {
		text = string([]rune(text)[:maxTitleRunes])
	}
	return text
}

// replyHintFor returns the out-of-band reply tool string for a provider.
// Telegraph never propagates Mayor replies back to the provider; this hint
// is what tells Mayor where to actually act. Returning empty string for an
// unknown provider keeps the field optional rather than emitting an empty
// header.
func replyHintFor(provider string) string {
	switch provider {
	case "jira":
		return "out-of-band only — use Jira MCP tools (e.g. mcp__atlassian__addCommentToJiraIssue, mcp__atlassian__transitionJiraIssue); do NOT reply via bead"
	case "github":
		return "out-of-band only — use `gh` CLI or GitHub MCP (e.g. gh pr comment, gh pr review); do NOT reply via bead"
	default:
		return ""
	}
}

// sanitizeHeaderValue strips CR and LF characters from a value that will be
// written into a header line. A newline in a header value would allow
// injection of arbitrary header lines into the mail body.
func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// buildBody constructs the full mail body: Telegraph-* metadata headers, an
// optional OPERATOR PROMPT block (when promptText is non-empty), and a delimited
// external-content block. The external text is capped at bodyCap bytes to limit
// context-injection surface.
func (t *Transformer) buildBody(event *telegraph.NormalizedEvent, promptText string) string {
	var b strings.Builder

	// Metadata block — all values come from NormalizedEvent structured fields.
	// sanitizeHeaderValue strips CR/LF to prevent header-injection via untrusted fields.
	b.WriteString("Telegraph-Transport: http-webhook\n")
	fmt.Fprintf(&b, "Telegraph-Provider: %s\n", sanitizeHeaderValue(event.Provider))
	fmt.Fprintf(&b, "Telegraph-Event-Type: %s\n", sanitizeHeaderValue(event.EventType))
	fmt.Fprintf(&b, "Telegraph-Event-ID: %s\n", sanitizeHeaderValue(event.EventID))
	fmt.Fprintf(&b, "Telegraph-Timestamp: %s\n", event.Timestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Telegraph-Actor: %s\n", sanitizeHeaderValue(event.Actor))
	fmt.Fprintf(&b, "Telegraph-Subject: %s\n", sanitizeHeaderValue(event.Subject))
	if event.CanonicalURL != "" {
		fmt.Fprintf(&b, "Telegraph-URL: %s\n", sanitizeHeaderValue(event.CanonicalURL))
	}
	if len(event.Labels) > 0 {
		sanitizedLabels := make([]string, len(event.Labels))
		for i, l := range event.Labels {
			sanitizedLabels[i] = sanitizeHeaderValue(l)
		}
		fmt.Fprintf(&b, "Telegraph-Labels: %s\n", strings.Join(sanitizedLabels, ", "))
	}
	// Telegraph is one-way. Replying inside the Mayor mail thread does NOT
	// propagate back to the provider — there is no return-channel translator
	// in the pipeline. Always emit a per-provider reply hint so Mayor reaches
	// for the correct out-of-band tool instead of dropping a bead reply
	// that goes nowhere.
	if hint := replyHintFor(event.Provider); hint != "" {
		fmt.Fprintf(&b, "Telegraph-Reply-Via: %s\n", hint)
	}

	// Operator prompt block — emitted only when a prompt resolved.
	// Sits between metadata headers and external content.
	if promptText != "" {
		fmt.Fprintf(&b, "\n--- OPERATOR PROMPT (trusted) ---\n%s\n--- END OPERATOR PROMPT ---", promptText)
	}

	// External content block with explicit trust delimiter.
	// Provider and Actor are sanitized to prevent delimiter spoofing.
	fmt.Fprintf(&b, "\n--- EXTERNAL CONTENT (untrusted: %s/%s) ---\n",
		sanitizeHeaderValue(event.Provider), sanitizeHeaderValue(event.Actor))

	text := event.Text
	if t.bodyCap > 0 && len(text) > t.bodyCap {
		text = text[:t.bodyCap] + "\n[… truncated]"
	}
	b.WriteString(text)

	b.WriteString("\n--- END EXTERNAL CONTENT ---\n")

	return b.String()
}
