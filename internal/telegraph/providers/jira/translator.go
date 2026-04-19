// Package jira implements the Telegraph L2 Translator for Jira webhooks.
//
// Supported event types (v1):
//
//	jira:issue_created   → issue.created
//	jira:issue_updated   → issue.updated
//	jira:comment_added   → comment.added
//	jira:comment_updated → comment.updated
//
// All other event types are rejected with ErrUnknownEventType and logged.
// The caller (L1) must return HTTP 200 on ErrUnknownEventType to prevent
// Jira from retrying indefinitely on unsupported types.
package jira

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

const providerID = "jira"

// supported maps Jira webhookEvent values to normalized dot-separated event types.
var supported = map[string]string{
	"jira:issue_created":  "issue.created",
	"jira:issue_updated":  "issue.updated",
	"jira:comment_added":  "comment.added",
	"jira:comment_updated": "comment.updated",
}

// Translator implements telegraph.Translator for Jira webhook payloads.
type Translator struct {
	secret []byte // HMAC-SHA256 signing secret; never logged
}

// New creates a Jira Translator with the given HMAC signing secret.
// secret must be non-empty; callers should fail fast at startup if it is not set.
func New(secret string) *Translator {
	return &Translator{secret: []byte(secret)}
}

// Provider returns the stable provider identifier.
func (t *Translator) Provider() string { return providerID }

// Authenticate verifies the Jira HMAC-SHA256 request signature.
// Jira sends the signature in the X-Hub-Signature header as "sha256=<hex>".
func (t *Translator) Authenticate(headers map[string]string, body []byte) error {
	sig, ok := headers["x-hub-signature"]
	if !ok {
		return fmt.Errorf("jira: missing X-Hub-Signature header")
	}

	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return fmt.Errorf("jira: X-Hub-Signature has unexpected format")
	}
	gotHex := sig[len(prefix):]

	want := computeHMAC(t.secret, body)
	got, err := hex.DecodeString(gotHex)
	if err != nil {
		return fmt.Errorf("jira: X-Hub-Signature is not valid hex")
	}
	if !hmac.Equal(want, got) {
		return fmt.Errorf("jira: HMAC signature mismatch")
	}
	return nil
}

// computeHMAC returns HMAC-SHA256(key, data).
func computeHMAC(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// jiraPayload is the minimal Jira webhook envelope used for parsing.
// Fields not needed for L2 output are omitted.
type jiraPayload struct {
	ID           int64      `json:"id"`
	Timestamp    int64      `json:"timestamp"` // milliseconds epoch
	WebhookEvent string     `json:"webhookEvent"`
	User         jiraUser   `json:"user"`
	Issue        jiraIssue  `json:"issue"`
	Comment      *jiraComment `json:"comment"`
}

type jiraUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

type jiraIssue struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Self   string      `json:"self"` // REST API URL; used to derive base URL
	Fields jiraFields  `json:"fields"`
}

type jiraFields struct {
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Labels      []string   `json:"labels"`
}

type jiraComment struct {
	ID      string   `json:"id"`
	Author  jiraUser `json:"author"`
	Body    string   `json:"body"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
}

// Translate parses a Jira webhook body and returns a NormalizedEvent.
// Returns ErrUnknownEventType for out-of-scope event types (logged, not dropped).
func (t *Translator) Translate(body []byte) (*telegraph.NormalizedEvent, error) {
	var p jiraPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("jira: failed to parse webhook body: %w", err)
	}

	eventType, ok := supported[p.WebhookEvent]
	if !ok {
		eventID := eventIDFromPayload(p)
		log.Printf(`{"component":"telegraph","event":"reject","provider":"jira","reason":"unknown_event_type","webhook_event":%q,"event_id":%q}`,
			p.WebhookEvent, eventID)
		return nil, telegraph.ErrUnknownEventType
	}

	actor := actorHandle(p)
	subject := p.Issue.Key
	canonicalURL := canonicalIssueURL(p.Issue)
	text := buildText(p, eventType)
	labels := p.Issue.Fields.Labels
	ts := eventTimestamp(p)

	return &telegraph.NormalizedEvent{
		Provider:     providerID,
		EventType:    eventType,
		EventID:      eventIDFromPayload(p),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: canonicalURL,
		Text:         text,
		Labels:       labels,
		Timestamp:    ts,
	}, nil
}

// actorHandle returns a stable, human-readable actor identifier.
// Prefers email address; falls back to displayName, then accountId.
func actorHandle(p jiraPayload) string {
	u := p.User
	if u.EmailAddress != "" {
		return u.EmailAddress
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.AccountID
}

// canonicalIssueURL derives the Jira UI browse URL from the issue's REST self link.
// e.g. "https://company.atlassian.net/rest/api/2/issue/10001" →
//
//	"https://company.atlassian.net/browse/PROJ-1234"
func canonicalIssueURL(issue jiraIssue) string {
	if issue.Self == "" || issue.Key == "" {
		return ""
	}
	u, err := url.Parse(issue.Self)
	if err != nil {
		return ""
	}
	u.Path = "/browse/" + issue.Key
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// buildText produces the salient text for the NormalizedEvent.
// For comment events, returns the comment body.
// For issue events, returns the summary (plus truncated description if present).
func buildText(p jiraPayload, eventType string) string {
	if strings.HasPrefix(eventType, "comment.") && p.Comment != nil {
		return p.Comment.Body
	}
	summary := p.Issue.Fields.Summary
	desc := p.Issue.Fields.Description
	if desc == "" {
		return summary
	}
	const descSnippetMax = 200
	if len(desc) > descSnippetMax {
		desc = desc[:descSnippetMax] + "…"
	}
	return summary + "\n" + desc
}

// eventTimestamp converts the Jira millisecond epoch to UTC time.
// Falls back to zero time if the field is absent.
func eventTimestamp(p jiraPayload) time.Time {
	if p.Timestamp == 0 {
		return time.Time{}
	}
	return time.UnixMilli(p.Timestamp).UTC()
}

// eventIDFromPayload returns a string event ID from the numeric webhook ID.
func eventIDFromPayload(p jiraPayload) string {
	if p.ID == 0 {
		return ""
	}
	return fmt.Sprintf("%d", p.ID)
}
