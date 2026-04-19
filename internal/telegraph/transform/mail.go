// Package transform implements Telegraph L3: the provider-agnostic transformation
// layer that converts NormalizedEvents into Mayor-addressed mail envelopes and
// delivers them with rate-limited nudging.
package transform

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/telegraph"
)

const (
	// mayorAddress is the fixed mail recipient for all Telegraph events (v1).
	mayorAddress = "mayor/"

	// nudgeMessage is the generic nudge sent to Mayor after mail delivery.
	// Keep it generic — Mayor reads actual event details from mail.
	nudgeMessage = "Telegraph: new events pending"

	// senderPrefix is the stable From address prefix for Telegraph.
	// Full format: telegraph/<provider>/<actor>
	senderPrefix = "telegraph"

	// externalContentOpen is the opening delimiter wrapping untrusted external text.
	externalContentOpen = "--- EXTERNAL CONTENT (untrusted: %s/%s) ---"

	// externalContentClose is the closing delimiter for untrusted external text.
	externalContentClose = "--- END EXTERNAL CONTENT ---"

	// truncationNotice appended inside the content block when body is capped.
	truncationNotice = "\n[… truncated]"
)

// Deliverer delivers NormalizedEvents to Mayor as structured mail with
// rate-limited nudging. It is safe for concurrent use.
type Deliverer struct {
	townRoot    string
	workDir     string
	nudgeWindow time.Duration
	bodyCap     int

	mu        sync.Mutex
	lastNudge time.Time
}

// NewDeliverer creates a Deliverer for the given town root.
// nudgeWindow controls the minimum interval between Mayor nudges.
// bodyCap caps the external content block size in bytes; 0 uses the default.
func NewDeliverer(townRoot, workDir string, nudgeWindow time.Duration, bodyCap int) *Deliverer {
	if bodyCap <= 0 {
		bodyCap = telegraph.DefaultBodyCap
	}
	return &Deliverer{
		townRoot:    townRoot,
		workDir:     workDir,
		nudgeWindow: nudgeWindow,
		bodyCap:     bodyCap,
	}
}

// Deliver converts a NormalizedEvent to a Mayor-addressed mail message and sends it.
// After delivery it conditionally sends a rate-limited nudge to Mayor.
// Returns the mail bead ID on success.
func (d *Deliverer) Deliver(ev *telegraph.NormalizedEvent) (string, error) {
	msg := d.buildEnvelope(ev)

	router := mail.NewRouterWithTownRoot(d.workDir, d.townRoot)
	if err := router.Send(msg); err != nil {
		return "", fmt.Errorf("telegraph deliver: %w", err)
	}

	mailID := msg.ID
	d.maybeNudge()
	return mailID, nil
}

// buildEnvelope constructs the mail.Message for a NormalizedEvent following
// the contract defined in docs/design/telegraph.md.
func (d *Deliverer) buildEnvelope(ev *telegraph.NormalizedEvent) *mail.Message {
	from := buildFrom(ev.Provider, ev.Actor)
	subject := buildSubject(ev)
	body := d.buildBody(ev)

	msg := mail.NewMessage(from, mayorAddress, subject, body)
	msg.Priority = mail.PriorityNormal
	msg.Type = mail.TypeNotification
	return msg
}

// buildFrom returns the stable From address for a Telegraph event.
// Format: telegraph/<provider>/<actor>
func buildFrom(provider, actor string) string {
	return fmt.Sprintf("%s/%s/%s", senderPrefix, provider, actor)
}

// buildSubject constructs the Subject line from structured NormalizedEvent fields.
// Never uses raw user text — only provider, subject entity, and event type.
// Format: [<PROVIDER> <SUBJECT>] <EventType prose>: <salient delta>
func buildSubject(ev *telegraph.NormalizedEvent) string {
	providerUpper := strings.ToUpper(ev.Provider)
	prefix := fmt.Sprintf("[%s %s]", providerUpper, ev.Subject)
	prose := eventTypeProse(ev.EventType, ev.Actor)
	return fmt.Sprintf("%s %s", prefix, prose)
}

// eventTypeProse converts a dot-separated event type to a human-readable summary.
// Input comes from NormalizedEvent.EventType which is controlled by L2, not user text.
func eventTypeProse(eventType, actor string) string {
	switch eventType {
	case "issue.created":
		return "Issue created"
	case "issue.updated":
		return "Issue updated"
	case "comment.added":
		return fmt.Sprintf("Comment added by %s", actor)
	case "comment.updated":
		return fmt.Sprintf("Comment updated by %s", actor)
	default:
		// Unknown type: use a sanitized version of the type string.
		// eventType comes from L2 (controlled), not from raw user input.
		return fmt.Sprintf("Event: %s", eventType)
	}
}

// buildBody constructs the structured mail body with Telegraph-* metadata headers
// followed by the external content wrapped in trust-boundary delimiters.
//
// Format:
//
//	Telegraph-Transport: http-webhook
//	Telegraph-Provider: <provider>
//	...
//
//	--- EXTERNAL CONTENT (untrusted: <provider>/<actor>) ---
//	<Text field — capped at bodyCap bytes>
//	--- END EXTERNAL CONTENT ---
func (d *Deliverer) buildBody(ev *telegraph.NormalizedEvent) string {
	var sb strings.Builder

	// Metadata block (Telegraph-* headers). All values come from structured
	// NormalizedEvent fields — never from raw user text.
	sb.WriteString("Telegraph-Transport: http-webhook\n")
	sb.WriteString(fmt.Sprintf("Telegraph-Provider: %s\n", ev.Provider))
	sb.WriteString(fmt.Sprintf("Telegraph-Event-Type: %s\n", ev.EventType))
	sb.WriteString(fmt.Sprintf("Telegraph-Event-ID: %s\n", ev.EventID))
	sb.WriteString(fmt.Sprintf("Telegraph-Timestamp: %s\n", ev.Timestamp.UTC().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Telegraph-Actor: %s\n", ev.Actor))
	sb.WriteString(fmt.Sprintf("Telegraph-Subject: %s\n", ev.Subject))
	sb.WriteString(fmt.Sprintf("Telegraph-URL: %s\n", ev.CanonicalURL))
	if len(ev.Labels) > 0 {
		sb.WriteString(fmt.Sprintf("Telegraph-Labels: %s\n", strings.Join(ev.Labels, ", ")))
	}

	// External content block.
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf(externalContentOpen, ev.Provider, ev.Actor))
	sb.WriteString("\n")

	text := capText(ev.Text, d.bodyCap)
	sb.WriteString(text)
	if utf8.RuneCountInString(ev.Text) > utf8.RuneCountInString(text) {
		sb.WriteString(truncationNotice)
	}

	sb.WriteString("\n")
	sb.WriteString(externalContentClose)
	sb.WriteString("\n")

	return sb.String()
}

// capText truncates text to at most maxBytes bytes, preserving valid UTF-8.
// Returns text unchanged if it is within the limit.
func capText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	// Truncate at a valid UTF-8 boundary.
	truncated := text[:maxBytes]
	for !utf8.ValidString(truncated) && len(truncated) > 0 {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// maybeNudge sends a rate-limited nudge to Mayor if the nudge window has elapsed.
// Uses a mutex to safely track lastNudge across concurrent goroutines.
// The nudge is best-effort; errors are silently ignored (Mayor discovers mail
// on next startup via gt mail inbox).
func (d *Deliverer) maybeNudge() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if now.Sub(d.lastNudge) < d.nudgeWindow {
		return
	}

	session := mayorSessionID(d.townRoot)
	if session == "" {
		return
	}

	_ = nudge.Enqueue(d.townRoot, session, nudge.QueuedNudge{
		Sender:   senderPrefix,
		Message:  nudgeMessage,
		Priority: nudge.PriorityNormal,
	})
	d.lastNudge = now
}

// mayorSessionID returns the tmux session ID for Mayor.
// Uses the canonical Mail address-to-session conversion.
func mayorSessionID(townRoot string) string {
	ids := mail.AddressToSessionIDs(mayorAddress)
	if len(ids) == 0 {
		return ""
	}
	_ = townRoot // townRoot reserved for future session-registry lookup
	return ids[0]
}

// DeliverLog is a structured log record for a successful delivery.
// Callers are responsible for writing this to the configured log destination.
type DeliverLog struct {
	Ts        string `json:"ts"`
	Component string `json:"component"`
	Event     string `json:"event"`
	Provider  string `json:"provider"`
	EventType string `json:"event_type"`
	EventID   string `json:"event_id"`
	Actor     string `json:"actor"`
	Subject   string `json:"subject"`
	MailID    string `json:"mail_id"`
}

// NewDeliverLog creates a DeliverLog for a successful mail delivery.
func NewDeliverLog(ev *telegraph.NormalizedEvent, mailID string) *DeliverLog {
	return &DeliverLog{
		Ts:        time.Now().UTC().Format(time.RFC3339),
		Component: "telegraph",
		Event:     "deliver",
		Provider:  ev.Provider,
		EventType: ev.EventType,
		EventID:   ev.EventID,
		Actor:     ev.Actor,
		Subject:   ev.Subject,
		MailID:    mailID,
	}
}

// MarshalJSON returns the JSON encoding of the log record.
func (l *DeliverLog) MarshalJSON() ([]byte, error) {
	type alias DeliverLog
	return json.Marshal((*alias)(l))
}

// Format returns the single-line JSON log string for this delivery event.
func (l *DeliverLog) Format() string {
	data, _ := json.Marshal(l)
	return string(data)
}

