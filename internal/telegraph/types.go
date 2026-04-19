// Package telegraph implements the town-level inbound external event transport.
// It converts external provider webhooks (Jira, etc.) into Mayor-addressed mail.
//
// Three-layer architecture:
//   - L1 (this package + transport/): HTTP listener, auth, enqueue
//   - L2 (providers/): per-provider translation to NormalizedEvent
//   - L3 (transform/): mail envelope builder + rate-limited Mayor nudge
package telegraph

import (
	"errors"
	"time"
)

// ErrUnknownEventType is returned by Translator.Translate for event types
// outside the provider's configured scope. The caller logs and rejects;
// it never silently drops.
var ErrUnknownEventType = errors.New("unknown event type")

// RawEvent is the authenticated-but-uninterpreted payload from Transport (L1).
// L1 guarantees the request passed HMAC/signature verification before enqueuing.
// L2 never re-verifies; it only translates.
type RawEvent struct {
	Provider   string            // stable provider ID, e.g. "jira"
	Headers    map[string]string // original HTTP headers (lowercased keys)
	Body       []byte            // raw request body (must not be mutated)
	SourceIP   string            // remote addr for logging
	ReceivedAt time.Time
}

// NormalizedEvent is the provider-agnostic representation produced by L2.
// L3 consumes this; it knows nothing about Jira or any other provider.
type NormalizedEvent struct {
	Provider     string    // e.g. "jira"
	EventType    string    // dot-separated, e.g. "issue.created", "comment.updated"
	EventID      string    // provider-native event ID (for dedup logging only)
	Actor        string    // who triggered the event (stable user handle)
	Subject      string    // primary entity, e.g. "PROJ-1234"
	CanonicalURL string    // link back to entity in provider UI
	Text         string    // salient text: title + description snippet or comment body
	Labels       []string  // provider-native labels/tags (not instructions)
	Timestamp    time.Time // event time from provider (UTC)
}

// Translator is implemented once per provider. L1 selects the right Translator
// by matching the request path segment or provider config to Provider().
type Translator interface {
	// Provider returns the stable provider identifier ("jira", "github", …).
	Provider() string

	// Authenticate verifies the request signature or token.
	// Called by L1 before enqueuing. Returns non-nil on failure.
	// Must not log secrets.
	Authenticate(headers map[string]string, body []byte) error

	// Translate converts a raw body to a NormalizedEvent.
	// Returns ErrUnknownEventType if the event type is not in scope.
	// Unknown types MUST be logged (with EventID if extractable) and returned
	// as ErrUnknownEventType — never silently dropped, never forwarded raw.
	Translate(body []byte) (*NormalizedEvent, error)
}

// Dispatcher receives RawEvents from L1 and routes them through L2→L3.
// The default implementation in this package is a no-op stub; the daemon
// wires a real dispatcher that chains L2 translators and L3 transform.
type Dispatcher interface {
	// Dispatch processes a RawEvent. It is called from the dispatchLoop goroutine.
	// Returning an error causes a structured reject log entry.
	Dispatch(ev RawEvent) error
}

// LogFunc is the structured log emitter used throughout telegraph.
// The daemon passes its logger.Printf here.
type LogFunc func(format string, args ...any)
