package telegraph

import (
	"errors"
	"time"
)

// ErrUnknownEventType is returned by Translate for out-of-scope event types.
var ErrUnknownEventType = errors.New("unknown event type")

// RawEvent is the authenticated-but-uninterpreted payload from Transport.
// L1 guarantees the request passed HMAC/signature verification before
// enqueuing. L2 never re-verifies; it only translates.
type RawEvent struct {
	Provider   string
	Headers    map[string]string // lowercased keys
	Body       []byte
	SourceIP   string
	ReceivedAt time.Time
}

// NormalizedEvent is the provider-agnostic representation produced by L2.
// L3 consumes this; it knows nothing about Jira or any other provider.
type NormalizedEvent struct {
	Provider     string
	EventType    string    // dot-separated, e.g. "issue.created"
	EventID      string    // provider-native event ID (for dedup logging only)
	Actor        string    // who triggered the event
	Subject      string    // primary entity, e.g. "PROJ-1234"
	CanonicalURL string    // link back to entity in provider UI
	Text         string    // salient text: title + description snippet or comment body
	Labels       []string  // provider-native labels/tags (not instructions)
	Timestamp    time.Time // event time from provider (UTC)
}

// Translator is implemented once per provider. L1 selects the right
// Translator by matching the request path to Provider().
type Translator interface {
	// Provider returns the stable provider identifier ("jira", "github", …).
	Provider() string

	// Authenticate verifies the request signature or token.
	// Called by L1 before enqueuing. Returns non-nil on failure.
	// Must not log secrets.
	Authenticate(headers map[string]string, body []byte) error

	// Translate converts a raw body to a NormalizedEvent.
	// Returns ErrUnknownEventType if the event type is not in scope.
	// Unknown types MUST be logged and returned as ErrUnknownEventType —
	// never silently dropped, never forwarded raw.
	Translate(body []byte) (*NormalizedEvent, error)
}
