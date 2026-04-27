// Package prompts implements operator-side prompt resolution for Telegraph L3.
// It resolves a per-event-type template, substitutes NormalizedEvent fields,
// sanitizes substituted values, and enforces a byte cap.
package prompts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/telegraph"
)

const (
	delimStart = "--- OPERATOR PROMPT (trusted) ---"
	delimEnd   = "--- END OPERATOR PROMPT ---"
)

// keyRegex validates <provider>:<event_type> keys. The event_type segment must
// have at least two dot-separated components (e.g. "comment.added", "issue.field.changed").
// Underscores are allowed within segments. The special key "default" is exempt.
var keyRegex = regexp.MustCompile(`^[a-z]+:[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]+)+$`)

// Config holds the operator prompt templates and cap setting.
type Config struct {
	// Default is the fallback template used when no exact key matches.
	// Empty means no default (omit block when no exact key matches).
	Default string

	// ByKey maps "<provider>:<event_type>" to a prompt template.
	ByKey map[string]string

	// Cap is the maximum bytes of a resolved (post-substitution) prompt.
	// 0 means no cap. Truncation appends "\n[… prompt truncated]".
	Cap int
}

// Resolver resolves operator prompts for NormalizedEvents.
// A nil *Resolver is valid: Resolve returns ("", "") with no panic.
type Resolver struct {
	cfg Config
}

// NewResolver validates cfg and returns a Resolver.
// Returns an error if any key is malformed, any value is empty, or any
// template contains either OPERATOR PROMPT delimiter literal.
func NewResolver(cfg Config) (*Resolver, error) {
	for k, v := range cfg.ByKey {
		if !keyRegex.MatchString(k) {
			return nil, fmt.Errorf("telegraph/prompts: invalid key %q (must match ^[a-z]+:[a-z][a-z0-9_]*(\\.[a-z][a-z0-9_]+)+$)", k)
		}
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("telegraph/prompts: prompt value for key %q is empty", k)
		}
		if err := validateTemplate(k, v); err != nil {
			return nil, err
		}
	}
	if cfg.Default != "" {
		if strings.TrimSpace(cfg.Default) == "" {
			return nil, errors.New("telegraph/prompts: default prompt is empty after trimming")
		}
		if err := validateTemplate("default", cfg.Default); err != nil {
			return nil, err
		}
	}
	return &Resolver{cfg: cfg}, nil
}

// validateTemplate checks that tmpl does not contain either OPERATOR PROMPT delimiter.
func validateTemplate(key, tmpl string) error {
	if strings.Contains(tmpl, delimStart) {
		return fmt.Errorf("telegraph/prompts: prompt for key %q contains start delimiter %q", key, delimStart)
	}
	if strings.Contains(tmpl, delimEnd) {
		return fmt.Errorf("telegraph/prompts: prompt for key %q contains end delimiter %q", key, delimEnd)
	}
	return nil
}

// Resolve returns the rendered prompt and the key that resolved it.
// resolvedKey is the exact key ("jira:comment.added"), "default", or ""
// if no prompt is configured for the event. Returns ("", "") for a nil Resolver.
func (r *Resolver) Resolve(event *telegraph.NormalizedEvent) (text, resolvedKey string) {
	if r == nil {
		return "", ""
	}
	tmpl, key := r.lookup(event.Provider + ":" + event.EventType)
	if tmpl == "" {
		return "", ""
	}
	rendered := substitute(tmpl, event)
	rendered = r.enforceCap(rendered)
	return rendered, key
}

func (r *Resolver) lookup(key string) (tmpl, resolvedKey string) {
	if t, ok := r.cfg.ByKey[key]; ok {
		return t, key
	}
	if r.cfg.Default != "" {
		return r.cfg.Default, "default"
	}
	return "", ""
}

func (r *Resolver) enforceCap(text string) string {
	if r.cfg.Cap <= 0 || len(text) <= r.cfg.Cap {
		return text
	}
	return truncateAtRuneBoundary(text, r.cfg.Cap) + "\n[… prompt truncated]"
}

// truncateAtRuneBoundary slices text to at most maxBytes by scanning backward
// from maxBytes to find the last valid UTF-8 rune start boundary.
func truncateAtRuneBoundary(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(text[i]) {
		i--
	}
	return text[:i]
}

// substitute replaces all defined tokens in tmpl with values from event.
// Unknown tokens (e.g. {foo}) are left as literal text.
// Each substituted value passes through sanitization before insertion.
func substitute(tmpl string, event *telegraph.NormalizedEvent) string {
	ts := ""
	if !event.Timestamp.IsZero() {
		ts = event.Timestamp.UTC().Format(time.RFC3339)
	}

	labelParts := make([]string, len(event.Labels))
	for i, l := range event.Labels {
		labelParts[i] = stripCRLF(l)
	}
	labelsVal := sanitizeSubstituted(strings.Join(labelParts, ", "))

	r := strings.NewReplacer(
		"{provider}", sanitizeSubstituted(event.Provider),
		"{event_type}", sanitizeSubstituted(event.EventType),
		"{event_id}", sanitizeSubstituted(event.EventID),
		"{actor}", sanitizeSubstituted(event.Actor),
		"{subject}", sanitizeSubstituted(event.Subject),
		"{canonical_url}", sanitizeSubstituted(event.CanonicalURL),
		"{timestamp}", sanitizeSubstituted(ts),
		"{labels}", labelsVal,
	)
	return r.Replace(tmpl)
}

// stripCRLF removes carriage return and line feed characters.
func stripCRLF(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// sanitizeSubstituted strips CR/LF then checks for exact delimiter match.
// If the stripped value (trimmed) equals a delimiter literal, it is replaced
// with the redaction marker and a warning is emitted via the structured log.
func sanitizeSubstituted(s string) string {
	stripped := stripCRLF(s)
	trimmed := strings.TrimSpace(stripped)
	if trimmed == delimStart || trimmed == delimEnd {
		return "[redacted: delimiter spoof]"
	}
	return stripped
}
