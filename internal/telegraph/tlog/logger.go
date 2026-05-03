// Package tlog provides the shared structured logger for all Telegraph layers.
// Every terminal outcome emits a single JSON line to an io.Writer and increments
// an atomic counter. The counters are exported for health checks and tests.
package tlog

import (
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Reason constants for reject/drop events, matching §Observability in telegraph.md.
const (
	ReasonHMACInvalid      = "hmac_invalid"
	ReasonUnknownEventType = "unknown_event_type"
	ReasonParseError       = "parse_error"
	ReasonBackpressure     = "backpressure"
	ReasonProviderDisabled = "provider_disabled"
	ReasonActorFiltered    = "actor_filtered"
	ReasonRepoFiltered     = "repo_filtered"
)

// Counters holds one atomic counter per event class. Safe for concurrent use.
type Counters struct {
	Accept              atomic.Int64
	RejectHMACInvalid   atomic.Int64
	RejectUnknownType   atomic.Int64
	RejectParseError    atomic.Int64
	RejectBackpressure  atomic.Int64
	RejectProviderDis   atomic.Int64 // provider_disabled
	Deliver             atomic.Int64
	Drop                atomic.Int64
	NudgeSent           atomic.Int64
	NudgeSuppressed     atomic.Int64
}

// Logger emits structured JSON log lines and maintains event counters.
// A nil *Logger is valid: all methods are no-ops on nil, so callers may pass nil
// to disable logging without special-casing.
type Logger struct {
	w        io.Writer
	Counters Counters
}

// New creates a Logger writing to w.
func New(w io.Writer) *Logger {
	return &Logger{w: w}
}

// logEntry is the per-line JSON schema, per §Observability in docs/design/telegraph.md.
type logEntry struct {
	TS        string `json:"ts"`
	Component string `json:"component"`
	Event     string `json:"event"`
	Provider  string `json:"provider,omitempty"`
	SourceIP  string `json:"source_ip,omitempty"`
	EventType string `json:"event_type,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Subject   string `json:"subject,omitempty"`
	MailID    string `json:"mail_id,omitempty"`
	PromptKey string `json:"prompt_key,omitempty"`
	BytesLen  int    `json:"bytes_len,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
	Reason    string `json:"reason,omitempty"`
}

// Accept logs a successful authenticate+enqueue outcome from L1.
func (l *Logger) Accept(provider, sourceIP, eventID string, bytesLen int, latencyMs int64) {
	if l == nil {
		return
	}
	l.Counters.Accept.Add(1)
	l.emit(logEntry{
		Event:     "accept",
		Provider:  provider,
		SourceIP:  sourceIP,
		EventID:   eventID,
		BytesLen:  bytesLen,
		LatencyMs: latencyMs,
	})
}

// Reject logs a rejection. reason must be one of the Reason* constants.
// eventID may be empty when the body was unparseable.
func (l *Logger) Reject(provider, sourceIP, reason, eventID string) {
	if l == nil {
		return
	}
	switch reason {
	case ReasonHMACInvalid:
		l.Counters.RejectHMACInvalid.Add(1)
	case ReasonUnknownEventType:
		l.Counters.RejectUnknownType.Add(1)
	case ReasonParseError:
		l.Counters.RejectParseError.Add(1)
	case ReasonBackpressure:
		l.Counters.RejectBackpressure.Add(1)
	case ReasonProviderDisabled:
		l.Counters.RejectProviderDis.Add(1)
	}
	l.emit(logEntry{
		Event:    "reject",
		Provider: provider,
		SourceIP: sourceIP,
		Reason:   reason,
		EventID:  eventID,
	})
}

// Deliver logs a successful mail send to Mayor from L3.
// mailID is the bead ID of the created mail; may be empty if unavailable.
// promptKey is the resolved prompt template key ("jira:comment.added", "default",
// or "" if no operator prompt block was emitted).
func (l *Logger) Deliver(provider, eventType, eventID, actor, subject, mailID, promptKey string) {
	if l == nil {
		return
	}
	l.Counters.Deliver.Add(1)
	l.emit(logEntry{
		Event:     "deliver",
		Provider:  provider,
		EventType: eventType,
		EventID:   eventID,
		Actor:     actor,
		Subject:   subject,
		MailID:    mailID,
		PromptKey: promptKey,
	})
}

// Drop logs an event discarded after L2 without delivery.
// actor may be empty for drop reasons that do not have an actor (e.g. parse errors).
// For actor_filtered drops, actor must be populated for the audit trail.
// subject identifies the entity (e.g. Jira issue key, GitHub repo#PR), and is
// included in the audit log to distinguish "what was filtered" from logs alone;
// pass empty string for drop reasons where subject is unavailable.
func (l *Logger) Drop(provider, eventType, eventID, actor, subject, reason string) {
	if l == nil {
		return
	}
	l.Counters.Drop.Add(1)
	l.emit(logEntry{
		Event:     "drop",
		Provider:  provider,
		EventType: eventType,
		EventID:   eventID,
		Actor:     actor,
		Subject:   subject,
		Reason:    reason,
	})
}

// NudgeSent logs a Mayor nudge that was sent (within rate-limit policy).
func (l *Logger) NudgeSent() {
	if l == nil {
		return
	}
	l.Counters.NudgeSent.Add(1)
	l.emit(logEntry{Event: "nudge_sent"})
}

// NudgeSuppressed logs a Mayor nudge that was suppressed by the rate-limit window.
func (l *Logger) NudgeSuppressed() {
	if l == nil {
		return
	}
	l.Counters.NudgeSuppressed.Add(1)
	l.emit(logEntry{Event: "nudge_suppressed"})
}

func (l *Logger) emit(e logEntry) {
	e.TS = time.Now().UTC().Format(time.RFC3339)
	e.Component = "telegraph"
	data, err := json.Marshal(e)
	if err != nil {
		fmt.Fprintf(l.w, `{"component":"telegraph","event":"log_error","err":%q}`+"\n", err.Error())
		return
	}
	fmt.Fprintln(l.w, string(data))
}
