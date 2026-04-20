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

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/telegraph"
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

// ExecNudger runs "gt nudge <target> -m <message>" as a subprocess.
// It is the production Nudger.
type ExecNudger struct{}

// Nudge delivers a nudge by running gt nudge.
func (n *ExecNudger) Nudge(target, message string) error {
	return exec.Command("gt", "nudge", target, "-m", message).Run()
}

// Transformer is the L3 layer. It is safe for concurrent use.
type Transformer struct {
	sender      MailSender
	nudger      Nudger
	bodyCap     int
	nudgeWindow time.Duration

	mu        sync.Mutex
	lastNudge time.Time
}

// New returns a Transformer. bodyCap is the maximum bytes of external text
// written into the mail body; content beyond the cap is truncated. nudgeWindow
// is the minimum interval between consecutive Mayor nudges; zero disables nudges.
func New(sender MailSender, nudger Nudger, bodyCap int, nudgeWindow time.Duration) *Transformer {
	return &Transformer{
		sender:      sender,
		nudger:      nudger,
		bodyCap:     bodyCap,
		nudgeWindow: nudgeWindow,
	}
}

// NewProduction returns a Transformer wired to a production mail router and an
// exec-based nudger. townRoot is used to construct the Router's working directory.
func NewProduction(townRoot string, bodyCap int, nudgeWindow time.Duration) *Transformer {
	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	return New(router, &ExecNudger{}, bodyCap, nudgeWindow)
}

// Transform builds a Mayor-addressed mail envelope from event, sends it via the
// configured MailSender, and conditionally nudges Mayor within the rate-limit
// window. It is safe to call concurrently.
func (t *Transformer) Transform(event *telegraph.NormalizedEvent) error {
	msg := mail.NewMessage(
		buildFrom(event),
		"mayor/",
		buildSubject(event),
		t.buildBody(event),
	)

	if err := t.sender.Send(msg); err != nil {
		return fmt.Errorf("telegraph/transform: mail send: %w", err)
	}

	t.maybeNudge()
	return nil
}

// maybeNudge sends a nudge to Mayor if the rate-limit window has elapsed.
func (t *Transformer) maybeNudge() {
	if t.nudgeWindow <= 0 || t.nudger == nil {
		return
	}

	t.mu.Lock()
	now := time.Now()
	if now.Sub(t.lastNudge) < t.nudgeWindow {
		t.mu.Unlock()
		return
	}
	t.lastNudge = now
	t.mu.Unlock()

	// Nudge failure is non-fatal: Mayor will discover mail on next inbox check.
	_ = t.nudger.Nudge("mayor/", "Telegraph: new events pending")
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
	default:
		return fmt.Sprintf("%s %s", tag, event.EventType)
	}
}

// safeTitle returns at most the first line of text (no raw body content in subject).
func safeTitle(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	const maxTitleLen = 80
	if len(text) > maxTitleLen {
		text = text[:maxTitleLen]
	}
	return text
}

// buildBody constructs the full mail body: Telegraph-* metadata headers followed
// by a delimited external-content block. The external text is capped at bodyCap
// bytes to limit context-injection surface.
func (t *Transformer) buildBody(event *telegraph.NormalizedEvent) string {
	var b strings.Builder

	// Metadata block — all values come from NormalizedEvent structured fields.
	b.WriteString("Telegraph-Transport: http-webhook\n")
	fmt.Fprintf(&b, "Telegraph-Provider: %s\n", event.Provider)
	fmt.Fprintf(&b, "Telegraph-Event-Type: %s\n", event.EventType)
	fmt.Fprintf(&b, "Telegraph-Event-ID: %s\n", event.EventID)
	fmt.Fprintf(&b, "Telegraph-Timestamp: %s\n", event.Timestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Telegraph-Actor: %s\n", event.Actor)
	fmt.Fprintf(&b, "Telegraph-Subject: %s\n", event.Subject)
	if event.CanonicalURL != "" {
		fmt.Fprintf(&b, "Telegraph-URL: %s\n", event.CanonicalURL)
	}
	if len(event.Labels) > 0 {
		fmt.Fprintf(&b, "Telegraph-Labels: %s\n", strings.Join(event.Labels, ", "))
	}

	// External content block with explicit trust delimiter.
	fmt.Fprintf(&b, "\n--- EXTERNAL CONTENT (untrusted: %s/%s) ---\n", event.Provider, event.Actor)

	text := event.Text
	if t.bodyCap > 0 && len(text) > t.bodyCap {
		text = text[:t.bodyCap] + "\n[… truncated]"
	}
	b.WriteString(text)

	b.WriteString("\n--- END EXTERNAL CONTENT ---\n")

	return b.String()
}
