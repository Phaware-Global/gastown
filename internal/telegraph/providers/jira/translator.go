// Package jira implements the L2 Translator for Jira webhooks.
//
// Supported v1 event types:
//   jira:issue_created  → issue.created
//   jira:issue_updated  → issue.updated  (status, priority, assignee transitions)
//   jira:comment_added  → comment.added
//   jira:comment_updated → comment.updated
//
// All other event types are rejected with ErrUnknownEventType and logged.
// Authentication uses HMAC-SHA256 via the X-Hub-Signature header.
package jira

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

const providerID = "jira"

// knownEvents maps Jira webhookEvent strings to dot-notation EventType strings.
// Older Jira versions omit the "jira:" prefix on comment events; both are accepted.
var knownEvents = map[string]string{
	"jira:issue_created":   "issue.created",
	"jira:issue_updated":   "issue.updated",
	"jira:comment_added":   "comment.added",
	"jira:comment_updated": "comment.updated",
	"comment_created":      "comment.added",
	"comment_added":        "comment.added",
	"comment_updated":      "comment.updated",
}

// Translator implements telegraph.Translator for Jira webhooks.
type Translator struct {
	secret []byte
	logger *slog.Logger
}

// New creates a Jira Translator with the given HMAC-SHA256 secret.
// If logger is nil, slog.Default() is used.
func New(secret []byte, logger *slog.Logger) *Translator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Translator{secret: secret, logger: logger}
}

// Provider returns "jira".
func (t *Translator) Provider() string { return providerID }

// Authenticate verifies the X-Hub-Signature HMAC-SHA256 header.
// Returns non-nil if the header is missing, malformed, or the HMAC does not match.
// Secret values are never logged.
func (t *Translator) Authenticate(headers map[string]string, body []byte) error {
	sig := headers["x-hub-signature"]
	if sig == "" {
		return errors.New("missing X-Hub-Signature header")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return errors.New("unsupported signature scheme: want sha256=<hex>")
	}
	expected, err := hex.DecodeString(strings.TrimPrefix(sig, prefix))
	if err != nil {
		return fmt.Errorf("invalid HMAC hex encoding: %w", err)
	}
	mac := hmac.New(sha256.New, t.secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), expected) {
		return errors.New("HMAC signature mismatch")
	}
	return nil
}

// Translate converts a raw Jira webhook body into a NormalizedEvent.
// Returns ErrUnknownEventType for event types outside the v1 scope.
// JSON parse errors return a wrapped error with "parse_error:" prefix.
func (t *Translator) Translate(body []byte) (*telegraph.NormalizedEvent, error) {
	var raw jiraPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse_error: %w", err)
	}

	eventType, ok := knownEvents[raw.WebhookEvent]
	if !ok {
		t.logger.Info("telegraph: unknown jira event type — rejecting",
			"provider", providerID,
			"webhook_event", raw.WebhookEvent,
			"event_id", raw.eventIDStr(),
		)
		return nil, telegraph.ErrUnknownEventType
	}

	return &telegraph.NormalizedEvent{
		Provider:     providerID,
		EventType:    eventType,
		EventID:      raw.eventIDStr(),
		Actor:        raw.actorForEvent(eventType),
		Subject:      raw.Issue.Key,
		CanonicalURL: raw.canonicalURL(),
		Text:         raw.salientText(eventType),
		Labels:       raw.Issue.Fields.Labels,
		Timestamp:    raw.timestampUTC(),
	}, nil
}

// jiraPayload is the parsed form of a Jira webhook body.
type jiraPayload struct {
	ID           int64          `json:"id"`
	Timestamp    int64          `json:"timestamp"` // milliseconds since epoch
	WebhookEvent string         `json:"webhookEvent"`
	User         jiraUser       `json:"user"`
	Issue        jiraIssue      `json:"issue"`
	Comment      *jiraComment   `json:"comment"`
	Changelog    *jiraChangelog `json:"changelog"`
}

type jiraUser struct {
	Name        string `json:"name"`        // stable handle on Jira Server
	AccountID   string `json:"accountId"`   // stable handle on Jira Cloud
	DisplayName string `json:"displayName"` // human-readable fallback
}

type jiraIssue struct {
	ID     string     `json:"id"`
	Key    string     `json:"key"`    // e.g. "PROJ-1234"
	Self   string     `json:"self"`   // REST API URL for base URL extraction
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Labels      []string   `json:"labels"`
	Status      *jiraNamed `json:"status"`
	Priority    *jiraNamed `json:"priority"`
	Assignee    *jiraUser  `json:"assignee"`
}

type jiraNamed struct {
	Name string `json:"name"`
}

type jiraComment struct {
	ID     string   `json:"id"`
	Author jiraUser `json:"author"`
	Body   string   `json:"body"`
}

type jiraChangelog struct {
	ID    string              `json:"id"`
	Items []jiraChangelogItem `json:"items"`
}

type jiraChangelogItem struct {
	Field      string `json:"field"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

// eventIDStr returns the provider-native event ID as a string.
func (p *jiraPayload) eventIDStr() string {
	if p.ID == 0 {
		return ""
	}
	return fmt.Sprintf("%d", p.ID)
}

// timestampUTC converts the Jira millisecond timestamp to UTC time.Time.
// Falls back to the current time if the timestamp is zero (malformed payload).
func (p *jiraPayload) timestampUTC() time.Time {
	if p.Timestamp == 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(p.Timestamp).UTC()
}

// actorForEvent returns the stable user handle for the triggering actor.
// For comment events, the comment author is the actor; otherwise the top-level user.
func (p *jiraPayload) actorForEvent(eventType string) string {
	if (eventType == "comment.added" || eventType == "comment.updated") && p.Comment != nil {
		return userHandle(p.Comment.Author)
	}
	return userHandle(p.User)
}

// userHandle extracts the stable handle from a Jira user.
// Prefers name (Server), falls back to accountId (Cloud), then displayName.
func userHandle(u jiraUser) string {
	if u.Name != "" {
		return u.Name
	}
	if u.AccountID != "" {
		return u.AccountID
	}
	return u.DisplayName
}

// canonicalURL constructs the Jira browse URL from the issue's REST API self URL.
// e.g. "https://company.atlassian.net/rest/api/2/issue/10000" →
//
//	"https://company.atlassian.net/browse/PROJ-1234"
func (p *jiraPayload) canonicalURL() string {
	if p.Issue.Key == "" {
		return ""
	}
	u, err := url.Parse(p.Issue.Self)
	if err != nil || u.Host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s/browse/%s", u.Scheme, u.Host, p.Issue.Key)
}

// salientText returns the relevant text snippet for the given event type.
func (p *jiraPayload) salientText(eventType string) string {
	switch eventType {
	case "issue.created":
		return p.Issue.Fields.Summary
	case "issue.updated":
		return p.issueUpdatedText()
	case "comment.added", "comment.updated":
		if p.Comment != nil {
			return p.Comment.Body
		}
		return ""
	default:
		return p.Issue.Fields.Summary
	}
}

// issueUpdatedText builds a summary of what changed, appended to the issue summary.
// Format: "<summary>\n<field>: <from> → <to>; …"
func (p *jiraPayload) issueUpdatedText() string {
	if p.Changelog == nil || len(p.Changelog.Items) == 0 {
		return p.Issue.Fields.Summary
	}
	var parts []string
	for _, item := range p.Changelog.Items {
		if item.Field == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s → %s", item.Field, item.FromString, item.ToString))
	}
	if len(parts) == 0 {
		return p.Issue.Fields.Summary
	}
	return p.Issue.Fields.Summary + "\n" + strings.Join(parts, "; ")
}
