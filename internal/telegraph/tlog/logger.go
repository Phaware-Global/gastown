// Package tlog provides the shared structured logger for all Telegraph layers.
// Every terminal outcome emits a single JSON line to an io.Writer and increments
// an atomic counter. The counters are exported for health checks and tests.
package tlog

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
	ReasonNotRelevant      = "not_relevant"
	ReasonTranslateError   = "translate_error"
)

// Counters holds one atomic counter per event class. Safe for concurrent use.
type Counters struct {
	Accept             atomic.Int64
	RejectHMACInvalid  atomic.Int64
	RejectUnknownType  atomic.Int64
	RejectParseError   atomic.Int64
	RejectBackpressure atomic.Int64
	RejectProviderDis  atomic.Int64 // provider_disabled
	Deliver            atomic.Int64
	Drop               atomic.Int64
	NudgeSent          atomic.Int64
	NudgeSuppressed    atomic.Int64
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

	// Diagnostic fields used by translate_error drops.
	Err           string            `json:"err,omitempty"`
	WireEvent     string            `json:"wire_event,omitempty"`
	DeliveryID    string            `json:"delivery_id,omitempty"`
	BodySnippet   string            `json:"body_snippet,omitempty"`
	BodyTruncated bool              `json:"body_truncated,omitempty"`
	SafeHeaders   map[string]string `json:"safe_headers,omitempty"`
}

// translateErrorBodyCap bounds how much of the offending payload is embedded
// in the structured log line. Webhook bodies are bounded at 1 MiB upstream
// (transport.bodyReadLimit); a 4 KiB cap keeps single log lines greppable
// and avoids ballooning the daemon log on a flapping translator. The full
// raw body is still available via the upstream provider; the snippet exists
// to make in-log debugging possible without external lookups.
const translateErrorBodyCap = 4096

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

// TranslateError logs a true translation failure (translator returned a
// non-sentinel error). Includes a UTF-8-safe snippet of the request body and
// a vetted subset of HTTP headers — never signature/auth/cookie headers —
// so an operator can debug the failure without re-deriving payload context
// from the upstream provider.
//
// Expected-drop paths (ErrUnknownEventType, ErrActorFiltered, ErrRepoFiltered,
// ErrNotRelevant) MUST NOT be routed through this method; they have their
// own Drop() reasons. Routing expected drops here would inflate the drop
// counter with false-positive translate_error noise — the exact regression
// this method exists to prevent.
//
// eventType / eventID are optional context the caller may know even when
// translation failed (e.g. wire_event for GitHub is known from the HTTP
// header before the JSON parse runs). Pass empty strings when unavailable.
func (l *Logger) TranslateError(provider, eventType, eventID string, headers map[string]string, body []byte, err error) {
	if l == nil {
		return
	}
	l.Counters.Drop.Add(1)
	snippet, truncated := safeBodySnippet(body, translateErrorBodyCap)
	entry := logEntry{
		Event:         "drop",
		Provider:      provider,
		EventType:     eventType,
		EventID:       eventID,
		Reason:        ReasonTranslateError,
		BytesLen:      len(body),
		BodySnippet:   snippet,
		BodyTruncated: truncated,
		SafeHeaders:   safeHeaders(headers),
	}
	if err != nil {
		entry.Err = err.Error()
	}
	if h, ok := headers["x-github-event"]; ok {
		entry.WireEvent = h
	}
	if h, ok := headers["x-github-delivery"]; ok {
		entry.DeliveryID = h
	}
	l.emit(entry)
}

// safeHeaderAllowlist enumerates HTTP headers that are safe to log. Signature,
// auth, and cookie headers are intentionally excluded — those are credentials,
// not diagnostics. New entries here MUST be reviewed for sensitivity.
var safeHeaderAllowlist = map[string]struct{}{
	"content-type":      {},
	"user-agent":        {},
	"x-github-event":    {},
	"x-github-delivery": {},
	"x-github-hook-id":  {},
	"x-jira-webhook":    {},
}

// safeHeaders returns a copy of headers limited to the allowlist. Unknown
// headers are dropped — explicit allowlist beats blocklist for this kind of
// sensitive-data filter. Header keys are expected to be lowercased (L1
// contract).
func safeHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(safeHeaderAllowlist))
	for k, v := range in {
		if _, ok := safeHeaderAllowlist[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// safeBodySnippet returns up to cap bytes of body as a UTF-8 string, plus a
// flag indicating whether the input was truncated. Invalid UTF-8 sequences
// (e.g. random bytes from a corrupted body) are replaced with U+FFFD so the
// emitted JSON remains valid.
func safeBodySnippet(body []byte, cap int) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	truncated := false
	if len(body) > cap {
		body = body[:cap]
		truncated = true
	}
	// Replace invalid UTF-8 so json.Marshal produces a valid line.
	if !isValidUTF8(body) {
		return strings.ToValidUTF8(string(body), "�"), truncated
	}
	return string(body), truncated
}

// isValidUTF8 wraps utf8.Valid so the imports remain colocated with the user.
func isValidUTF8(b []byte) bool {
	return utf8.Valid(b)
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
