// Package transform implements Telegraph L3: the provider-agnostic envelope
// builder and rate-limited Mayor nudge. It consumes NormalizedEvents from L2
// and delivers Mayor-addressed mail using the existing gt mail + nudge primitives.
//
// Adding a new provider does NOT require changes to this package.
package transform

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/telegraph"
)

const (
	// DefaultNudgeWindow is the default rate-limit window for Mayor nudges.
	DefaultNudgeWindow = 30 * time.Second

	// DefaultBodyCap is the default maximum bytes of external text in a mail body.
	DefaultBodyCap = 4096

	mayorAddress   = "mayor/"
	telegraphAgent = "telegraph"
)

// MailSender abstracts mail delivery for testing.
type MailSender interface {
	Send(msg *mail.Message) error
}

// nudgeFn is the signature for enqueuing a nudge to a session.
type nudgeFn func(sess string, n nudge.QueuedNudge) error

// Config holds L3 (Transformation) configuration.
type Config struct {
	// NudgeWindow is the minimum time between Mayor nudges (default 30s).
	NudgeWindow time.Duration

	// BodyCap is the maximum bytes of external text included in the mail body
	// before truncation (default 4096). Content beyond the cap is replaced with
	// a "[… truncated]" notice inside the delimited block.
	BodyCap int

	// Log is the structured logger. Nil defaults to stderr JSON.
	Log *slog.Logger
}

func (c Config) effectiveNudgeWindow() time.Duration {
	if c.NudgeWindow > 0 {
		return c.NudgeWindow
	}
	return DefaultNudgeWindow
}

func (c Config) effectiveBodyCap() int {
	if c.BodyCap > 0 {
		return c.BodyCap
	}
	return DefaultBodyCap
}

// Transformer builds Mayor-addressed mail envelopes from NormalizedEvents
// and delivers them with a rate-limited nudge.
type Transformer struct {
	cfg      Config
	townRoot string
	sender   MailSender
	nudger   nudgeFn
	log      *slog.Logger

	mu        sync.Mutex
	lastNudge time.Time
}

// New creates a Transformer that uses the production mail router and nudge queue.
func New(townRoot string, cfg Config) *Transformer {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	return &Transformer{
		cfg:      cfg,
		townRoot: townRoot,
		sender:   router,
		nudger: func(sess string, n nudge.QueuedNudge) error {
			return nudge.Enqueue(townRoot, sess, n)
		},
		log: log,
	}
}

// newWithDeps creates a Transformer with injected dependencies (for testing).
func newWithDeps(townRoot string, cfg Config, sender MailSender, n nudgeFn, log *slog.Logger) *Transformer {
	return &Transformer{
		cfg:      cfg,
		townRoot: townRoot,
		sender:   sender,
		nudger:   n,
		log:      log,
	}
}

// Deliver sends a NormalizedEvent as Mayor-addressed mail and emits a
// rate-limited nudge. It is safe to call concurrently.
func (t *Transformer) Deliver(_ context.Context, ev *telegraph.NormalizedEvent) error {
	from := buildFrom(ev)
	subject := buildSubject(ev)
	body := t.buildBody(ev)

	msg := mail.NewMessage(from, mayorAddress, subject, body)
	// Telegraph manages its own nudging via the rate-limiter below.
	// Suppress the router's automatic per-delivery notification to avoid
	// a nudge storm when many events arrive in a short window.
	msg.SuppressNotify = true

	if err := t.sender.Send(msg); err != nil {
		return fmt.Errorf("telegraph deliver: %w", err)
	}

	t.log.Info("deliver",
		slog.String("component", "telegraph"),
		slog.String("event", "deliver"),
		slog.String("provider", ev.Provider),
		slog.String("event_type", ev.EventType),
		slog.String("event_id", ev.EventID),
		slog.String("actor", ev.Actor),
		slog.String("subject", ev.Subject),
		slog.String("mail_id", msg.ID),
	)

	t.maybeNudge()
	return nil
}

// buildFrom constructs the From address per the mail envelope contract.
// Format: telegraph/<provider>/<actor>
func buildFrom(ev *telegraph.NormalizedEvent) string {
	return fmt.Sprintf("%s/%s/%s", telegraphAgent, ev.Provider, ev.Actor)
}

// buildSubject constructs the Subject from structured NormalizedEvent fields only.
// No user-supplied text (ev.Text) is used here — preventing subject-line injection.
//
// Format: [<PROVIDER> <Subject>] <EventType prose>: by <Actor>
func buildSubject(ev *telegraph.NormalizedEvent) string {
	providerUpper := strings.ToUpper(ev.Provider)
	eventProse := eventTypeProse(ev.EventType)
	return fmt.Sprintf("[%s %s] %s: by %s", providerUpper, ev.Subject, eventProse, ev.Actor)
}

// eventTypeProse converts a dot-separated event type to a human-readable phrase.
// "issue.created" → "Issue created"
// "comment.updated" → "Comment updated"
func eventTypeProse(eventType string) string {
	parts := strings.SplitN(eventType, ".", 2)
	noun := ucFirst(parts[0])
	if len(parts) == 2 {
		return noun + " " + parts[1]
	}
	return noun
}

func ucFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// buildBody constructs the structured mail body:
//  1. Telegraph-* metadata headers from NormalizedEvent structured fields
//  2. A delimited block of external content (ev.Text), capped at cfg.BodyCap bytes
//
// No user-supplied text appears outside the delimited block.
func (t *Transformer) buildBody(ev *telegraph.NormalizedEvent) string {
	var b strings.Builder

	// Metadata block — all structured fields, no user text
	b.WriteString("Telegraph-Transport: http-webhook\n")
	fmt.Fprintf(&b, "Telegraph-Provider: %s\n", ev.Provider)
	fmt.Fprintf(&b, "Telegraph-Event-Type: %s\n", ev.EventType)
	fmt.Fprintf(&b, "Telegraph-Event-ID: %s\n", ev.EventID)
	fmt.Fprintf(&b, "Telegraph-Timestamp: %s\n", ev.Timestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Telegraph-Actor: %s\n", ev.Actor)
	fmt.Fprintf(&b, "Telegraph-Subject: %s\n", ev.Subject)
	if ev.CanonicalURL != "" {
		fmt.Fprintf(&b, "Telegraph-URL: %s\n", ev.CanonicalURL)
	}
	if len(ev.Labels) > 0 {
		fmt.Fprintf(&b, "Telegraph-Labels: %s\n", strings.Join(ev.Labels, ", "))
	}

	// Delimited external content — explicitly marked as untrusted
	cap := t.cfg.effectiveBodyCap()
	text := ev.Text
	truncated := false
	if len(text) > cap {
		text = text[:cap]
		truncated = true
	}
	fmt.Fprintf(&b, "\n--- EXTERNAL CONTENT (untrusted: %s/%s) ---\n", ev.Provider, ev.Actor)
	b.WriteString(text)
	if truncated {
		b.WriteString("\n[… truncated]")
	}
	b.WriteString("\n--- END EXTERNAL CONTENT ---\n")

	return b.String()
}

// maybeNudge sends a rate-limited nudge to Mayor after a mail delivery.
// At most one nudge is sent per nudge_window regardless of event volume.
// The nudge is advisory: if Mayor is not running, the nudge is lost (acceptable;
// Mayor will discover the mail on next startup via gt mail inbox).
func (t *Transformer) maybeNudge() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if !t.lastNudge.IsZero() && now.Sub(t.lastNudge) < t.cfg.effectiveNudgeWindow() {
		return
	}

	sess := session.MayorSessionName()
	if err := t.nudger(sess, nudge.QueuedNudge{
		Sender:   telegraphAgent,
		Message:  "Telegraph: new events pending",
		Priority: nudge.PriorityNormal,
	}); err != nil {
		// Non-fatal: mail is already delivered; nudge is advisory
		t.log.Warn("telegraph nudge failed",
			slog.String("component", "telegraph"),
			slog.String("error", err.Error()),
		)
		return
	}
	t.lastNudge = now
}
