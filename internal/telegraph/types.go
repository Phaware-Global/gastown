// Package telegraph implements the town-level inbound external event transport.
// It converts external webhook events into Mayor-addressed mail with rate-limited nudges.
//
// Architecture: three layers
//   - L1 (Transport): HTTP webhook listener, authenticates via provider signature
//   - L2 (Translation): per-provider Translator converts RawEvent → NormalizedEvent
//   - L3 (Transformation): builds mail envelope, rate-limits nudges to Mayor
package telegraph

import (
	"errors"
	"time"
)

// ErrUnknownEventType is returned by Translate for out-of-scope event types.
// Unknown types must be logged and returned as this error — never silently dropped,
// never forwarded raw. L1 returns HTTP 200 to prevent provider retry storms.
var ErrUnknownEventType = errors.New("unknown event type")

// ErrActorFiltered is returned by Translate when the event's actor matches an
// entry in the provider's ignore_actors list. Unlike ErrUnknownEventType, the
// translator returns a non-nil NormalizedEvent alongside this error so the
// dispatcher can populate the audit-log line with actor/event_type/event_id.
// The dispatcher must not enqueue a filtered event to L3.
var ErrActorFiltered = errors.New("actor filtered by provider config")

// ErrRepoFiltered is returned by Translate when the event's source repository
// is not in the provider's allow-list. Like ErrActorFiltered, the translator
// returns a non-nil NormalizedEvent alongside this error so the dispatcher can
// populate the audit-log with subject/event_type/event_id. The dispatcher must
// not enqueue a filtered event to L3.
var ErrRepoFiltered = errors.New("repository filtered by provider config")

// RawEvent is the authenticated-but-uninterpreted payload from Transport.
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

// Translator is implemented once per provider. L1 selects the right
// Translator by matching the request path/header to Provider().
type Translator interface {
	// Provider returns the stable provider identifier ("jira", "github", …).
	Provider() string

	// Authenticate verifies the request signature or token.
	// Called by L1 before enqueuing. Returns non-nil on failure.
	// Must not log secrets.
	Authenticate(headers map[string]string, body []byte) error

	// Translate converts a raw body to a NormalizedEvent. Headers are the
	// authenticated request headers (lowercased keys); providers that select
	// the event type from a header (e.g. GitHub's X-GitHub-Event) read it from
	// here. Implementations that derive the event type entirely from the body
	// (e.g. Jira) may ignore headers.
	//
	// Returns ErrUnknownEventType if the event type is not in scope.
	// Unknown types MUST be logged (with EventID if extractable) and returned
	// as ErrUnknownEventType — never silently dropped, never forwarded raw.
	Translate(headers map[string]string, body []byte) (*NormalizedEvent, error)
}
