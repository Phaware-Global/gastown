// Package telegraph implements the three-layer inbound external event transport.
// See docs/design/telegraph.md for the full architecture.
package telegraph

import (
	"errors"
	"time"
)

// ErrUnknownEventType is returned by Translator.Translate for event types not in scope.
// L1 returns HTTP 200 to prevent the provider from retrying indefinitely.
var ErrUnknownEventType = errors.New("unknown event type")

// RawEvent is the authenticated-but-uninterpreted payload from L1 Transport.
// L1 guarantees the request passed HMAC/signature verification before enqueuing.
// L2 never re-verifies; it only translates.
type RawEvent struct {
	Provider   string            // stable provider ID, e.g. "jira"
	Headers    map[string]string // original HTTP headers (lowercased keys)
	Body       []byte            // raw request body (must not be mutated)
	SourceIP   string            // remote addr for logging
	ReceivedAt time.Time
}

// NormalizedEvent is the provider-agnostic representation produced by L2 Translation.
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
// by matching the request path or config to Provider().
//
// Adding a new provider requires only a new Translator implementation and a
// config stanza — no changes to L1 or L3.
type Translator interface {
	// Provider returns the stable provider identifier ("jira", "github", …).
	Provider() string

	// Authenticate verifies the request signature or token.
	// Called by L1 before enqueuing. Returns non-nil on failure.
	// Must not log or expose secret values.
	Authenticate(headers map[string]string, body []byte) error

	// Translate converts a raw body to a NormalizedEvent.
	// Returns ErrUnknownEventType if the event type is not in scope.
	// Unknown types MUST be logged (with EventID if extractable) and returned
	// as ErrUnknownEventType — never silently dropped, never forwarded raw.
	Translate(body []byte) (*NormalizedEvent, error)
}
