// Package telegraph defines shared types for the Telegraph event transport system.
//
// Three-layer architecture:
//   L1 (Transport)     — HTTP webhook listener; authenticates, enqueues RawEvent
//   L2 (Translation)   — per-provider Translator; converts RawEvent → NormalizedEvent
//   L3 (Transformation) — provider-agnostic; builds Mayor mail envelope + rate-limited nudge
//
// Adding a provider requires only a new L2 Translator implementation and config stanza.
// Layers 1 and 3 are unchanged.
package telegraph

import (
	"errors"
	"time"
)

// ErrUnknownEventType is returned by Translate for out-of-scope event types.
// L1 logs a reject entry and returns HTTP 200 to prevent provider retry storms.
var ErrUnknownEventType = errors.New("unknown event type")

// RawEvent is the authenticated-but-uninterpreted payload from L1 Transport.
// L1 guarantees the request passed HMAC/signature verification before enqueuing.
// L2 must not re-verify; it only translates.
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

// Translator is implemented once per provider.
// L1 selects the right Translator by matching the request path/header to Provider().
type Translator interface {
	// Provider returns the stable provider identifier ("jira", "github", …).
	Provider() string

	// Authenticate verifies the request signature or token.
	// Called by L1 before enqueuing. Returns non-nil on failure.
	// Must not log secret values.
	Authenticate(headers map[string]string, body []byte) error

	// Translate converts a raw body to a NormalizedEvent.
	// Returns ErrUnknownEventType if the event type is not in v1 scope.
	// Unknown types MUST be logged (with EventID if extractable) and returned
	// as ErrUnknownEventType — never silently dropped, never forwarded raw.
	Translate(body []byte) (*NormalizedEvent, error)
}
