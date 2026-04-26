// Package jira implements the Telegraph L2 Translator for Jira webhooks.
package jira

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// Translator implements telegraph.Translator for Jira webhook payloads.
type Translator struct {
	secret []byte
	logger *slog.Logger
}

// New creates a Jira Translator with the given HMAC secret.
// If logger is nil, slog.Default() is used.
func New(secret string, logger *slog.Logger) *Translator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Translator{
		secret: []byte(secret),
		logger: logger,
	}
}

// Provider returns the stable provider identifier.
func (t *Translator) Provider() string { return "jira" }

// Authenticate verifies the HMAC-SHA256 signature in the X-Hub-Signature header.
// Headers are expected to be lowercased (as guaranteed by L1).
func (t *Translator) Authenticate(headers map[string]string, body []byte) error {
	sig, ok := headers["x-hub-signature"]
	if !ok {
		return errors.New("missing x-hub-signature header")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return errors.New("x-hub-signature: expected sha256= prefix")
	}
	expectedBytes, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return fmt.Errorf("x-hub-signature: invalid hex: %w", err)
	}
	mac := hmac.New(sha256.New, t.secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), expectedBytes) {
		return errors.New("x-hub-signature: HMAC mismatch")
	}
	return nil
}

// Translate converts a Jira webhook body into a NormalizedEvent.
// Returns ErrUnknownEventType for unrecognised webhookEvent values.
func (t *Translator) Translate(body []byte) (*telegraph.NormalizedEvent, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("jira: parsing webhook payload: %w", err)
	}

	switch p.WebhookEvent {
	case "jira:issue_created":
		return translateIssueCreated(&p)
	case "jira:issue_updated":
		return translateIssueUpdated(&p)
	// Comment events: Jira's webhook API drops the `jira:` prefix and uses the
	// verb `created`/`updated` (not `added`/`updated`) for comment events. We
	// accept both the prefixed legacy form and the bare form Jira actually
	// sends in production. Discovered when a real comment_created webhook from
	// phaware.atlassian.net hit ErrUnknownEventType during dogfood.
	case "jira:comment_added", "comment_created":
		return translateCommentAdded(&p)
	case "jira:comment_updated", "comment_updated":
		return translateCommentUpdated(&p)
	default:
		t.logger.Info("telegraph: unknown jira event type",
			"event_type", p.WebhookEvent,
			"event_id", bestEventID(&p),
			"issue_key", issueKey(&p),
			"has_comment", p.Comment != nil,
			"has_issue", p.Issue != nil,
		)
		return nil, telegraph.ErrUnknownEventType
	}
}

// ---- internal payload types ----

type payload struct {
	Timestamp    int64      `json:"timestamp"` // milliseconds since epoch
	WebhookEvent string     `json:"webhookEvent"`
	User         *jiraUser  `json:"user"`
	Issue        *jiraIssue `json:"issue"`
	Comment      *comment   `json:"comment"`
	Changelog    *changelog `json:"changelog"`
}

type jiraUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type jiraIssue struct {
	Key    string       `json:"key"`
	Self   string       `json:"self"`
	Fields *issueFields `json:"fields"`
}

type issueFields struct {
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Labels      []string    `json:"labels"`
	Status      *namedField `json:"status"`
	Priority    *namedField `json:"priority"`
	Assignee    *jiraUser   `json:"assignee"`
}

type namedField struct {
	Name string `json:"name"`
}

type comment struct {
	ID           string    `json:"id"`
	Self         string    `json:"self"`
	Author       *jiraUser `json:"author"`
	UpdateAuthor *jiraUser `json:"updateAuthor"`
	Body         string    `json:"body"`
	Created      string    `json:"created"`
	Updated      string    `json:"updated"`
}

type changelog struct {
	Items []changeItem `json:"items"`
}

type changeItem struct {
	Field      string `json:"field"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

// ---- translators ----

func translateIssueCreated(p *payload) (*telegraph.NormalizedEvent, error) {
	if p.Issue == nil {
		return nil, fmt.Errorf("jira:issue_created: missing issue field")
	}
	return &telegraph.NormalizedEvent{
		Provider:     "jira",
		EventType:    "issue.created",
		EventID:      issueEventID(p),
		Actor:        userName(p.User),
		Subject:      p.Issue.Key,
		CanonicalURL: browseURL(p.Issue.Self, p.Issue.Key),
		Text:         issueText(p.Issue.Fields),
		Labels:       labels(p.Issue.Fields),
		Timestamp:    msToTime(p.Timestamp),
	}, nil
}

func translateIssueUpdated(p *payload) (*telegraph.NormalizedEvent, error) {
	if p.Issue == nil {
		return nil, fmt.Errorf("jira:issue_updated: missing issue field")
	}
	return &telegraph.NormalizedEvent{
		Provider:     "jira",
		EventType:    "issue.updated",
		EventID:      issueEventID(p),
		Actor:        userName(p.User),
		Subject:      p.Issue.Key,
		CanonicalURL: browseURL(p.Issue.Self, p.Issue.Key),
		Text:         changelogTextOrSummary(p.Changelog, p.Issue.Fields),
		Labels:       labels(p.Issue.Fields),
		Timestamp:    msToTime(p.Timestamp),
	}, nil
}

func translateCommentAdded(p *payload) (*telegraph.NormalizedEvent, error) {
	if p.Issue == nil {
		return nil, fmt.Errorf("jira:comment_added: missing issue field")
	}
	if p.Comment == nil {
		return nil, fmt.Errorf("jira:comment_added: missing comment field")
	}
	ts := parseJiraTime(p.Comment.Created)
	if ts.IsZero() {
		ts = msToTime(p.Timestamp)
	}
	return &telegraph.NormalizedEvent{
		Provider:     "jira",
		EventType:    "comment.added",
		EventID:      p.Comment.ID,
		Actor:        userName(p.Comment.Author),
		Subject:      p.Issue.Key,
		CanonicalURL: browseURL(p.Issue.Self, p.Issue.Key),
		Text:         p.Comment.Body,
		Labels:       labels(p.Issue.Fields),
		Timestamp:    ts,
	}, nil
}

func translateCommentUpdated(p *payload) (*telegraph.NormalizedEvent, error) {
	if p.Issue == nil {
		return nil, fmt.Errorf("jira:comment_updated: missing issue field")
	}
	if p.Comment == nil {
		return nil, fmt.Errorf("jira:comment_updated: missing comment field")
	}
	actor := userName(p.Comment.UpdateAuthor)
	if actor == "" {
		actor = userName(p.Comment.Author)
	}
	ts := parseJiraTime(p.Comment.Updated)
	if ts.IsZero() {
		ts = msToTime(p.Timestamp)
	}
	return &telegraph.NormalizedEvent{
		Provider:     "jira",
		EventType:    "comment.updated",
		EventID:      p.Comment.ID,
		Actor:        actor,
		Subject:      p.Issue.Key,
		CanonicalURL: browseURL(p.Issue.Self, p.Issue.Key),
		Text:         p.Comment.Body,
		Labels:       labels(p.Issue.Fields),
		Timestamp:    ts,
	}, nil
}

// ---- helpers ----

func userName(u *jiraUser) string {
	if u == nil {
		return ""
	}
	if u.Name != "" {
		return u.Name
	}
	return u.DisplayName
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

// parseJiraTime handles Jira's "2006-01-02T15:04:05.000+0000" timestamp format.
func parseJiraTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, f := range []string{
		"2006-01-02T15:04:05.000-0700",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func issueText(f *issueFields) string {
	if f == nil {
		return ""
	}
	if f.Description != "" {
		return f.Summary + "\n\n" + f.Description
	}
	return f.Summary
}

func changelogText(cl *changelog) string {
	if cl == nil || len(cl.Items) == 0 {
		return ""
	}
	var parts []string
	for _, item := range cl.Items {
		switch item.Field {
		case "status", "priority", "assignee", "summary":
			if item.FromString != "" {
				parts = append(parts, fmt.Sprintf("%s: %s → %s", item.Field, item.FromString, item.ToString))
			} else if item.ToString != "" {
				parts = append(parts, fmt.Sprintf("%s: %s", item.Field, item.ToString))
			}
		}
	}
	return strings.Join(parts, "; ")
}

// changelogTextOrSummary returns changelogText if non-empty, otherwise the issue summary.
func changelogTextOrSummary(cl *changelog, f *issueFields) string {
	if t := changelogText(cl); t != "" {
		return t
	}
	if f != nil {
		return f.Summary
	}
	return ""
}

// browseURL derives the user-facing browse URL from the Jira REST API self URL and issue key.
// Input:  https://company.atlassian.net/rest/api/2/issue/99291
// Output: https://company.atlassian.net/browse/PROJ-1234
func browseURL(self, key string) string {
	if self == "" || key == "" {
		return self
	}
	if idx := strings.Index(self, "/rest/"); idx >= 0 {
		return self[:idx] + "/browse/" + key
	}
	return self
}

func labels(f *issueFields) []string {
	if f == nil || len(f.Labels) == 0 {
		return []string{}
	}
	return f.Labels
}

func issueEventID(p *payload) string {
	if p.Issue == nil {
		return ""
	}
	if p.Timestamp != 0 {
		return fmt.Sprintf("%s-%d", p.Issue.Key, p.Timestamp)
	}
	return p.Issue.Key
}

func bestEventID(p *payload) string {
	if p.Comment != nil && p.Comment.ID != "" {
		return p.Comment.ID
	}
	return issueEventID(p)
}

func issueKey(p *payload) string {
	if p.Issue == nil {
		return ""
	}
	return p.Issue.Key
}
