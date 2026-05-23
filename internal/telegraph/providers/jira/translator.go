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
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// MayorIdentity identifies the mayor user for Jira relevance filtering.
//
//   - AccountIDs: opaque Jira Cloud account IDs. Matched exactly against
//     issue assignee.accountId, comment author.accountId, and the
//     accountid: form of @-mentions in comment bodies.
//   - Usernames: matched case-insensitively against User.name and
//     User.displayName, and against the bracketed [~username] form of
//     @-mentions used by Jira Server / legacy ADF payloads.
//
// An empty identity (no AccountIDs and no Usernames) disables relevance
// filtering — every successfully translated event is forwarded. This is
// the test-only and library-caller seam; production deployments go
// through telegraph.Config.Validate(), which refuses an empty identity
// when the jira provider is enabled.
type MayorIdentity struct {
	AccountIDs []string
	Usernames  []string
}

// HasAny reports whether the identity has at least one usable match target.
// Whitespace-only / empty entries are ignored so a slice like `[]string{""}`
// — which the construction-time normalization would discard — does not flip
// HasAny on and enable relevance filtering without any actual match
// targets behind it.
func (m MayorIdentity) HasAny() bool {
	for _, s := range m.AccountIDs {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	for _, s := range m.Usernames {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// Translator implements telegraph.Translator for Jira webhook payloads.
type Translator struct {
	secret        []byte
	ignoreActors  map[string]struct{}
	mayor         MayorIdentity
	mayorAccounts map[string]struct{} // case-sensitive (Jira account IDs are opaque)
	mayorUsersLC  map[string]struct{} // lower-cased
	logger        *slog.Logger
}

// New creates a Jira Translator with the given HMAC secret and actor filter list.
// ignoreActors is a list of actor display-names to silently drop; nil or empty disables filtering.
//
// mayor is the mayor user identity used by relevance filtering. When the
// identity is empty, relevance filtering is disabled (every successfully
// translated event is forwarded); when it is non-empty, events that do not
// pertain to the mayor are returned with ErrNotRelevant.
//
// If logger is nil, slog.Default() is used.
func New(secret string, ignoreActors []string, mayor MayorIdentity, logger *slog.Logger) *Translator {
	if logger == nil {
		logger = slog.Default()
	}
	set := make(map[string]struct{}, len(ignoreActors))
	for _, a := range ignoreActors {
		set[a] = struct{}{}
	}
	accounts := make(map[string]struct{}, len(mayor.AccountIDs))
	for _, id := range mayor.AccountIDs {
		if id != "" {
			accounts[id] = struct{}{}
		}
	}
	users := make(map[string]struct{}, len(mayor.Usernames))
	for _, u := range mayor.Usernames {
		if u != "" {
			users[strings.ToLower(u)] = struct{}{}
		}
	}
	return &Translator{
		secret:        []byte(secret),
		ignoreActors:  set,
		mayor:         mayor,
		mayorAccounts: accounts,
		mayorUsersLC:  users,
		logger:        logger,
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
// The headers parameter is unused — Jira encodes the event type in the JSON
// body's webhookEvent field, not in an HTTP header.
//
// Returns ErrUnknownEventType for unrecognised webhookEvent values.
// Returns (non-nil event, ErrActorFiltered) when the event's actor matches
// the ignore_actors list; the caller must log a drop and not enqueue to L3.
// Returns (non-nil event, ErrNotRelevant) when a known event type does not
// pertain to the mayor (no assignment, no mention, not on a mayor-assigned
// issue). The caller must audit-log the drop and not enqueue to L3.
func (t *Translator) Translate(_ map[string]string, body []byte) (*telegraph.NormalizedEvent, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("jira: parsing webhook payload: %w", err)
	}

	var (
		evt *telegraph.NormalizedEvent
		err error
	)
	switch p.WebhookEvent {
	case "jira:issue_created":
		evt, err = translateIssueCreated(&p)
	case "jira:issue_updated":
		evt, err = translateIssueUpdated(&p)
	// Comment events: Jira's webhook API drops the `jira:` prefix and uses the
	// verb `created` (instead of `added`) for creation events. We accept both
	// the prefixed legacy form and the bare form Jira actually sends in
	// production. Discovered during dogfooding when bare comment events
	// triggered ErrUnknownEventType.
	case "jira:comment_added", "comment_created":
		evt, err = translateCommentAdded(&p)
	case "jira:comment_updated", "comment_updated":
		evt, err = translateCommentUpdated(&p)
	default:
		t.logger.Info("telegraph: unknown jira event type",
			"event_type", p.WebhookEvent,
			"event_id", bestEventID(&p),
			"issue_key", issueKey(&p),
			"has_comment", p.Comment != nil,
			"has_issue", p.Issue != nil,
			"has_changelog", p.Changelog != nil,
		)
		return nil, telegraph.ErrUnknownEventType
	}
	if err != nil {
		return nil, err
	}
	if len(t.ignoreActors) > 0 {
		if _, filtered := t.ignoreActors[evt.Actor]; filtered {
			return evt, telegraph.ErrActorFiltered
		}
	}
	if t.mayor.HasAny() && !t.isRelevant(&p) {
		return evt, telegraph.ErrNotRelevant
	}
	return evt, nil
}

// isRelevant reports whether the parsed Jira payload pertains to the mayor.
//
// Relevance rules (any one is sufficient):
//  1. The issue is currently assigned to mayor — covers issue_created and
//     issue_updated where the mayor is the assignee, plus comment events on
//     a mayor-assigned issue.
//  2. The event is an assignment changelog whose toString resolves to mayor —
//     covers "assign to mayor" even before fields.assignee reflects it (the
//     changelog item is the canonical source of truth for who gained the
//     assignment).
//  3. A comment body explicitly @-mentions mayor (by accountId or username).
//
// Anything else — comments on others' issues, status updates on others'
// issues, new issues assigned to someone else — is irrelevant for the mayor.
func (t *Translator) isRelevant(p *payload) bool {
	// Rule 1: assignee on the issue matches mayor.
	if t.matchesUser(currentAssignee(p)) {
		return true
	}

	// Rule 2: assignment changelog → mayor. Cloud puts the new assignee's
	// accountId in `to` and the display name in `toString`; legacy/Server
	// puts the username in both. Match against either set so an operator
	// who configured only jira_account_ids still catches assignments.
	if to, toString := assignmentTargetFromChangelog(p); to != "" || toString != "" {
		if to != "" {
			if _, ok := t.mayorAccounts[to]; ok {
				return true
			}
			// Legacy Server: `to` is a username, not an accountId.
			if _, ok := t.mayorUsersLC[strings.ToLower(to)]; ok {
				return true
			}
		}
		if t.matchesUserName(toString) {
			return true
		}
	}

	// Rule 3: comment body @-mentions mayor.
	if p.Comment != nil {
		if t.commentMentionsMayor(p.Comment.Body) {
			return true
		}
	}
	return false
}

// matchesUser reports whether the given user (assignee, author, etc.) is the
// mayor. Compares accountId (case-sensitive, opaque) against AccountIDs, and
// name / displayName (case-insensitive) against Usernames.
func (t *Translator) matchesUser(u *jiraUser) bool {
	if u == nil {
		return false
	}
	if u.AccountID != "" {
		if _, ok := t.mayorAccounts[u.AccountID]; ok {
			return true
		}
	}
	if u.Name != "" {
		if _, ok := t.mayorUsersLC[strings.ToLower(u.Name)]; ok {
			return true
		}
	}
	if u.DisplayName != "" {
		if _, ok := t.mayorUsersLC[strings.ToLower(u.DisplayName)]; ok {
			return true
		}
	}
	return false
}

// matchesUserName reports whether a bare display-name string (e.g. from a
// changelog "toString") resolves to the mayor's username set. AccountIDs are
// never matched here because changelog toString carries the human-readable
// label, not the opaque ID.
func (t *Translator) matchesUserName(name string) bool {
	if name == "" {
		return false
	}
	_, ok := t.mayorUsersLC[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// commentMentionsMayor scans a Jira comment body for @-mention markers
// resolving to mayor. Jira renders mentions in two main forms:
//
//   - [~accountid:712020:abcd-...] — Cloud account-ID form (current)
//   - [~username]                  — legacy / Server form
//
// Both forms are detected with simple anchored substring matches. Display
// names of mentioned users are not embedded in the wire payload (Jira
// substitutes them at render time), so we cannot match displayName mentions
// here — accountId / username covers the production cases.
func (t *Translator) commentMentionsMayor(body string) bool {
	if body == "" {
		return false
	}
	matches := jiraMentionRE.FindAllStringSubmatch(body, -1)
	for _, m := range matches {
		// m[1] is the captured token; may be an accountid:... form or a bare
		// username. Jira's UI inserts mention markup when the user @-mentions
		// someone — the bracket form is never typed by hand — so we match the
		// token verbatim.
		token := m[1]
		if strings.HasPrefix(token, "accountid:") {
			id := strings.TrimPrefix(token, "accountid:")
			if _, ok := t.mayorAccounts[id]; ok {
				return true
			}
			continue
		}
		if _, ok := t.mayorUsersLC[strings.ToLower(token)]; ok {
			return true
		}
	}
	return false
}

// jiraMentionRE captures the inner token of a [~...] mention. The token is
// either "accountid:<opaque-id>" (cloud) or a bare username (server/legacy).
// Restricted to a permissive but bounded character class so we don't read
// past the closing bracket on adversarial input.
var jiraMentionRE = regexp.MustCompile(`\[~([^\]]+)\]`)

// currentAssignee returns the issue's current assignee, if present.
func currentAssignee(p *payload) *jiraUser {
	if p == nil || p.Issue == nil || p.Issue.Fields == nil {
		return nil
	}
	return p.Issue.Fields.Assignee
}

// assignmentTargetFromChangelog returns the most recent assignee change
// from the payload's changelog, or a zero struct if no such item is present.
//
// Jira's changelog is ordered oldest-first; the *last* matching item is the
// final assignee state at the moment the webhook fired. The structured
// result carries both the opaque "to" value (accountId on Cloud, username
// on legacy/Server) and the human-readable "toString" so the relevance
// check can match either AccountIDs- or Usernames-only mayor configs.
func assignmentTargetFromChangelog(p *payload) (to, toString string) {
	if p == nil || p.Changelog == nil {
		return "", ""
	}
	for i := len(p.Changelog.Items) - 1; i >= 0; i-- {
		it := p.Changelog.Items[i]
		if it.Field == "assignee" {
			return it.To, it.ToString
		}
	}
	return "", ""
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
	AccountID   string `json:"accountId"`
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
	// From / To carry the opaque value of the change. For assignee changes on
	// Jira Cloud these are accountIds; on legacy/Server they are usernames.
	From string `json:"from"`
	To   string `json:"to"`
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
		return nil, fmt.Errorf("comment_added: missing issue field")
	}
	if p.Comment == nil {
		return nil, fmt.Errorf("comment_added: missing comment field")
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
		return nil, fmt.Errorf("comment_updated: missing issue field")
	}
	if p.Comment == nil {
		return nil, fmt.Errorf("comment_updated: missing comment field")
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
