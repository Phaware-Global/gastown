package jira_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/providers/jira"
)

const testSecret = "test-secret-value"

func makeHMAC(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newTranslator() *jira.Translator {
	return jira.New(testSecret, nil, jira.MayorIdentity{}, nil)
}

// ---- Authenticate ----

func TestAuthenticate_Valid(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := []byte(`{"webhookEvent":"jira:issue_created"}`)
	headers := map[string]string{"x-hub-signature": makeHMAC(body)}
	if err := tr.Authenticate(headers, body); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
}

func TestAuthenticate_MissingHeader(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	err := tr.Authenticate(map[string]string{}, []byte("body"))
	if err == nil {
		t.Fatal("expected error for missing header")
	}
}

func TestAuthenticate_WrongSecret(t *testing.T) {
	t.Parallel()
	tr := jira.New("wrong-secret", nil, jira.MayorIdentity{}, nil)
	body := []byte(`{"webhookEvent":"jira:issue_created"}`)
	headers := map[string]string{"x-hub-signature": makeHMAC(body)}
	err := tr.Authenticate(headers, body)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestAuthenticate_BadPrefix(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	headers := map[string]string{"x-hub-signature": "md5=abc123"}
	err := tr.Authenticate(headers, []byte("body"))
	if err == nil {
		t.Fatal("expected error for non-sha256 prefix")
	}
}

func TestAuthenticate_InvalidHex(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	headers := map[string]string{"x-hub-signature": "sha256=notvalidhex!"}
	err := tr.Authenticate(headers, []byte("body"))
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

// ---- Provider ----

func TestProvider(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	if got := tr.Provider(); got != "jira" {
		t.Errorf("Provider() = %q, want jira", got)
	}
}

// ---- Translate helpers ----

type issuePayload struct {
	Timestamp    int64          `json:"timestamp"`
	WebhookEvent string         `json:"webhookEvent"`
	User         map[string]any `json:"user"`
	Issue        map[string]any `json:"issue"`
	Changelog    map[string]any `json:"changelog,omitempty"`
	Comment      map[string]any `json:"comment,omitempty"`
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func baseIssue(key string) map[string]any {
	return map[string]any{
		"key":  key,
		"self": "https://example.atlassian.net/rest/api/2/issue/10001",
		"fields": map[string]any{
			"summary":     "Test issue summary",
			"description": "Test description",
			"labels":      []string{"bug", "critical"},
			"status":      map[string]any{"name": "Open"},
			"priority":    map[string]any{"name": "High"},
		},
	}
}

func baseUser(name string) map[string]any {
	return map[string]any{"name": name, "displayName": "Display " + name}
}

// ---- issue_created ----

func TestTranslate_IssueCreated(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	tsMs := int64(1713571200000) // 2024-04-19 16:00:00 UTC
	body := mustMarshal(t, issuePayload{
		Timestamp:    tsMs,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("alice"),
		Issue:        baseIssue("PROJ-1"),
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.EventType != "issue.created" {
		t.Errorf("EventType = %q, want issue.created", evt.EventType)
	}
	if evt.Provider != "jira" {
		t.Errorf("Provider = %q, want jira", evt.Provider)
	}
	if evt.Actor != "alice" {
		t.Errorf("Actor = %q, want alice", evt.Actor)
	}
	if evt.Subject != "PROJ-1" {
		t.Errorf("Subject = %q, want PROJ-1", evt.Subject)
	}
	if evt.Text == "" {
		t.Error("Text is empty")
	}
	if len(evt.Labels) != 2 {
		t.Errorf("Labels len = %d, want 2", len(evt.Labels))
	}
	wantTS := time.Unix(tsMs/1000, 0).UTC()
	if !evt.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", evt.Timestamp, wantTS)
	}
}

func TestTranslate_IssueCreated_MissingIssue(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{"webhookEvent": "jira:issue_created"})
	_, err := tr.Translate(nil, body)
	if err == nil {
		t.Fatal("expected error for missing issue field")
	}
}

// ---- issue_updated ----

func TestTranslate_IssueUpdated(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("bob"),
		Issue:        baseIssue("PROJ-2"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "status", "fromString": "Open", "toString": "In Progress"},
				{"field": "priority", "fromString": "Low", "toString": "High"},
			},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.EventType != "issue.updated" {
		t.Errorf("EventType = %q, want issue.updated", evt.EventType)
	}
	if evt.Actor != "bob" {
		t.Errorf("Actor = %q, want bob", evt.Actor)
	}
	if evt.Text == "" {
		t.Error("Text (changelog) is empty")
	}
}

func TestTranslate_IssueUpdated_EmptyChangelog(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("bob"),
		Issue:        baseIssue("PROJ-3"),
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	// No tracked changelog fields → falls back to issue summary
	if evt.Text != "Test issue summary" {
		t.Errorf("Text = %q, want issue summary fallback", evt.Text)
	}
}

// ---- comment_added ----

func TestTranslate_CommentAdded(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "jira:comment_added",
		"user":         baseUser("carol"),
		"issue":        baseIssue("PROJ-4"),
		"comment": map[string]any{
			"id":      "999",
			"self":    "https://example.atlassian.net/rest/api/2/issue/10001/comment/999",
			"author":  baseUser("carol"),
			"body":    "This is a comment.",
			"created": "2024-04-19T16:00:00.000+0000",
			"updated": "2024-04-19T16:00:00.000+0000",
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.EventType != "comment.added" {
		t.Errorf("EventType = %q, want comment.added", evt.EventType)
	}
	if evt.Actor != "carol" {
		t.Errorf("Actor = %q, want carol", evt.Actor)
	}
	if evt.Text != "This is a comment." {
		t.Errorf("Text = %q, want comment body", evt.Text)
	}
	if evt.EventID != "999" {
		t.Errorf("EventID = %q, want 999", evt.EventID)
	}
}

// Jira's actual webhook API drops the `jira:` prefix and uses the verb
// `created` rather than `added` for comment-creation events. The bare form
// is what production Jira instances send.
func TestTranslate_CommentCreated_BareForm(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "comment_created",
		"user":         baseUser("carol"),
		"issue":        baseIssue("PROJ-42"),
		"comment": map[string]any{
			"id":      "777",
			"author":  baseUser("carol"),
			"body":    "Production-style comment.",
			"created": "2024-04-19T16:00:00.000+0000",
			"updated": "2024-04-19T16:00:00.000+0000",
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.EventType != "comment.added" {
		t.Errorf("EventType = %q, want comment.added (bare comment_created routes to same handler as jira:comment_added)", evt.EventType)
	}
	if evt.EventID != "777" {
		t.Errorf("EventID = %q, want 777", evt.EventID)
	}
	if evt.Subject != "PROJ-42" {
		t.Errorf("Subject = %q, want PROJ-42", evt.Subject)
	}
}

func TestTranslate_CommentAdded_MissingComment(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"webhookEvent": "jira:comment_added",
		"issue":        baseIssue("PROJ-5"),
	})
	_, err := tr.Translate(nil, body)
	if err == nil {
		t.Fatal("expected error for missing comment field")
	}
}

// ---- comment_updated ----

func TestTranslate_CommentUpdated(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "jira:comment_updated",
		"issue":        baseIssue("PROJ-6"),
		"comment": map[string]any{
			"id":           "888",
			"author":       baseUser("dave"),
			"updateAuthor": baseUser("eve"),
			"body":         "Updated comment text.",
			"created":      "2024-04-19T15:00:00.000+0000",
			"updated":      "2024-04-19T16:00:00.000+0000",
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.EventType != "comment.updated" {
		t.Errorf("EventType = %q, want comment.updated", evt.EventType)
	}
	if evt.Actor != "eve" {
		t.Errorf("Actor = %q, want eve (updateAuthor)", evt.Actor)
	}
	if evt.Text != "Updated comment text." {
		t.Errorf("Text = %q, want updated body", evt.Text)
	}
}

// Bare-name comment_updated (no `jira:` prefix) — same Jira API quirk as
// comment_created. Routes to the comment-updated handler.
func TestTranslate_CommentUpdated_BareForm(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "comment_updated",
		"issue":        baseIssue("PROJ-43"),
		"comment": map[string]any{
			"id":           "888",
			"author":       baseUser("dave"),
			"updateAuthor": baseUser("eve"),
			"body":         "Edited comment.",
			"created":      "2024-04-19T15:00:00.000+0000",
			"updated":      "2024-04-19T16:30:00.000+0000",
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.EventType != "comment.updated" {
		t.Errorf("EventType = %q, want comment.updated", evt.EventType)
	}
	if evt.Actor != "eve" {
		t.Errorf("Actor = %q, want eve (updateAuthor)", evt.Actor)
	}
}

// ---- unknown event type ----

func TestTranslate_UnknownEventType(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"webhookEvent": "jira:sprint_started",
		"issue":        baseIssue("PROJ-7"),
	})
	_, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("Translate() error = %v, want ErrUnknownEventType", err)
	}
}

// ---- parse errors ----

func TestTranslate_InvalidJSON(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	_, err := tr.Translate(nil, []byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Error("parse error should not be ErrUnknownEventType")
	}
}

// ---- CanonicalURL is browse URL, not REST API URL ----

func TestTranslate_CanonicalURL_IsBrowseURL(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("alice"),
		Issue:        baseIssue("PROJ-1"),
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	want := "https://example.atlassian.net/browse/PROJ-1"
	if evt.CanonicalURL != want {
		t.Errorf("CanonicalURL = %q, want %q", evt.CanonicalURL, want)
	}
}

func TestTranslate_CanonicalURL_NoRestPath(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	issue := map[string]any{
		"key":    "PROJ-9",
		"self":   "https://example.atlassian.net/browse/PROJ-9",
		"fields": map[string]any{"summary": "Already browse URL"},
	}
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("alice"),
		Issue:        issue,
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	// No /rest/ prefix — self returned unchanged
	if evt.CanonicalURL != "https://example.atlassian.net/browse/PROJ-9" {
		t.Errorf("CanonicalURL = %q", evt.CanonicalURL)
	}
}

// ---- issue_updated falls back to summary when changelog yields no tracked fields ----

func TestTranslate_IssueUpdated_FallbackToSummary(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("bob"),
		Issue:        baseIssue("PROJ-10"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "attachment", "fromString": "", "toString": "file.png"},
			},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.Text != "Test issue summary" {
		t.Errorf("Text = %q, want issue summary as fallback", evt.Text)
	}
}

// ---- summary field tracked in changelog ----

func TestTranslate_IssueUpdated_SummaryInChangelog(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("bob"),
		Issue:        baseIssue("PROJ-11"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "summary", "fromString": "Old title", "toString": "New title"},
			},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if !strings.Contains(evt.Text, "summary") {
		t.Errorf("Text = %q, want summary change reflected", evt.Text)
	}
	if !strings.Contains(evt.Text, "Old title") || !strings.Contains(evt.Text, "New title") {
		t.Errorf("Text = %q, want old→new summary values", evt.Text)
	}
}

// ---- labels default to empty slice (not nil) ----

func TestTranslate_IssueCreated_NoLabels(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	issue := map[string]any{
		"key":  "PROJ-8",
		"self": "https://example.atlassian.net/rest/api/2/issue/10008",
		"fields": map[string]any{
			"summary": "No labels",
		},
	}
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("frank"),
		Issue:        issue,
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.Labels == nil {
		t.Error("Labels should not be nil — must be empty slice")
	}
}

// ---- Actor filtering ----

// mustMarshalAny marshals v to JSON, panicking on error.
// Used for test data that doesn't have a *testing.T in scope.
func mustMarshalAny(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestActorFilter_EmptyIgnoreActors_NoFilter(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, []string{}, jira.MayorIdentity{}, nil)
	body := mustMarshalAny(issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:comment_added",
		Issue:        baseIssue("PROJ-10"),
		Comment: map[string]any{
			"id":      "cmt-1",
			"body":    "hello",
			"created": "2026-04-19T12:00:00.000+0000",
			"author":  map[string]string{"name": "Artie"},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v, want nil (no filter configured)", err)
	}
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
}

func TestActorFilter_NonMatchingActor_NoFilter(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, []string{"Artie"}, jira.MayorIdentity{}, nil)
	body := mustMarshalAny(issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:comment_added",
		Issue:        baseIssue("PROJ-11"),
		Comment: map[string]any{
			"id":      "cmt-2",
			"body":    "hello",
			"created": "2026-04-19T12:00:00.000+0000",
			"author":  map[string]string{"name": "alice"},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v, want nil (actor not in filter list)", err)
	}
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
}

func TestActorFilter_MatchingActor_CommentAdded(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, []string{"Artie"}, jira.MayorIdentity{}, nil)
	body := mustMarshalAny(issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:comment_added",
		Issue:        baseIssue("PROJ-12"),
		Comment: map[string]any{
			"id":      "cmt-3",
			"body":    "my comment",
			"created": "2026-04-19T12:00:00.000+0000",
			"author":  map[string]string{"name": "Artie"},
		},
	})
	evt, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrActorFiltered) {
		t.Fatalf("Translate() error = %v, want ErrActorFiltered", err)
	}
	// Non-nil event is required so dispatcher can log actor/eventType/eventID.
	if evt == nil {
		t.Fatal("Translate() returned nil event with ErrActorFiltered — event must be non-nil for audit log")
	}
	if evt.Actor != "Artie" {
		t.Errorf("event.Actor = %q, want Artie", evt.Actor)
	}
}

func TestActorFilter_MatchingActor_IssueUpdated(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, []string{"Artie"}, jira.MayorIdentity{}, nil)
	body := mustMarshalAny(issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("Artie"),
		Issue:        baseIssue("PROJ-13"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "status", "fromString": "Open", "toString": "In Progress"},
			},
		},
	})
	evt, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrActorFiltered) {
		t.Fatalf("Translate() error = %v, want ErrActorFiltered for issue.updated", err)
	}
	if evt == nil {
		t.Fatal("event must be non-nil for audit log")
	}
}

// ---- Mayor relevance filtering ----

// withAssignee returns a base issue with an assignee block. accountId or name
// can be empty to exercise different match paths.
func issueWithAssignee(key, assigneeName, assigneeAccountID, assigneeDisplay string) map[string]any {
	issue := baseIssue(key)
	fields := issue["fields"].(map[string]any)
	fields["assignee"] = map[string]any{
		"name":        assigneeName,
		"displayName": assigneeDisplay,
		"accountId":   assigneeAccountID,
	}
	return issue
}

const (
	mayorAcctID   = "712020:abcd-1234"
	mayorUserName = "Artie"
)

// mayorJiraIdentity is reused across the relevance tests.
func mayorJiraIdentity() jira.MayorIdentity {
	return jira.MayorIdentity{
		AccountIDs: []string{mayorAcctID},
		Usernames:  []string{mayorUserName},
	}
}

func TestRelevance_NoMayorIdentity_AllRelevant(t *testing.T) {
	t.Parallel()
	// With an empty identity, the translator must not apply relevance filtering
	// — preserves backward compatibility for operators who don't configure mayor.
	tr := jira.New(testSecret, nil, jira.MayorIdentity{}, nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("someoneElse"),
		Issue:        baseIssue("PROJ-1000"),
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (relevance disabled with empty identity)", err)
	}
}

func TestRelevance_IssueCreated_AssignedToMayor_Delivered(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("alice"),
		Issue:        issueWithAssignee("PROJ-100", "", mayorAcctID, "Artie"),
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate: %v, want nil (mayor is the assignee)", err)
	}
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
}

func TestRelevance_IssueCreated_AssignedToOther_Dropped(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("alice"),
		Issue:        issueWithAssignee("PROJ-101", "", "712020:other-acct", "Someone Else"),
	})
	evt, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant", err)
	}
	if evt == nil {
		t.Fatal("ErrNotRelevant must return non-nil event for audit log")
	}
	if evt.Subject != "PROJ-101" {
		t.Errorf("Subject = %q, want PROJ-101 (audit context preserved)", evt.Subject)
	}
}

func TestRelevance_IssueUpdated_AssignmentChangelogToMayor_Delivered(t *testing.T) {
	t.Parallel()
	// The fields.assignee may not reflect the new assignment yet — the
	// changelog item is the canonical assignment signal.
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("alice"),
		Issue:        baseIssue("PROJ-102"), // no assignee in snapshot
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "assignee", "fromString": "Bob", "toString": "Artie"},
			},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate: %v, want nil (assigned to mayor via changelog)", err)
	}
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
}

func TestRelevance_IssueUpdated_ChangelogAccountIDMatch_Delivered(t *testing.T) {
	t.Parallel()
	// Operator configured ONLY jira_account_ids (no usernames). Jira Cloud
	// puts the new assignee's accountId in changelog.items[].to and the
	// display name in toString; we must match against `to` so this case
	// isn't silently dropped.
	tr := jira.New(testSecret, nil, jira.MayorIdentity{AccountIDs: []string{mayorAcctID}}, nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("alice"),
		Issue:        baseIssue("PROJ-300"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{
					"field":      "assignee",
					"fromString": "Bob",
					"toString":   "Artie", // display name only — would miss with username-only config
					"from":       "acct-bob",
					"to":         mayorAcctID, // accountId — what we want to match on
				},
			},
		},
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (accountId-only config must catch assignment by `to`)", err)
	}
}

func TestRelevance_IssueUpdated_ChangelogLastItemWins(t *testing.T) {
	t.Parallel()
	// Jira changelog is oldest-first. If multiple assignee items exist, the
	// last one is the final assignee state at webhook fire time. Mayor was
	// assigned then immediately unassigned → the event should NOT be
	// classified as a mayor-assignment.
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("alice"),
		Issue:        baseIssue("PROJ-301"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "assignee", "fromString": "Bob", "toString": "Artie", "to": mayorAcctID},
				{"field": "assignee", "fromString": "Artie", "toString": "Carol", "to": "acct-carol"},
			},
		},
	})
	_, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate err = %v, want ErrNotRelevant (final assignee is Carol)", err)
	}
}

func TestRelevance_IssueUpdated_AssignmentChangelogToOther_Dropped(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("alice"),
		Issue:        baseIssue("PROJ-103"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "assignee", "fromString": "Artie", "toString": "Bob"},
			},
		},
	})
	_, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant (unassigned from mayor)", err)
	}
}

func TestRelevance_IssueUpdated_OnMayorAssignedIssue_Delivered(t *testing.T) {
	t.Parallel()
	// Non-assignment update on an issue already assigned to mayor.
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_updated",
		User:         baseUser("alice"),
		Issue:        issueWithAssignee("PROJ-104", "", mayorAcctID, "Artie"),
		Changelog: map[string]any{
			"items": []map[string]any{
				{"field": "status", "fromString": "Open", "toString": "Done"},
			},
		},
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (status change on mayor-assigned issue)", err)
	}
}

func TestRelevance_CommentOnMayorAssignedIssue_Delivered(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "comment_created",
		"issue":        issueWithAssignee("PROJ-105", "", mayorAcctID, "Artie"),
		"comment": map[string]any{
			"id":     "c-1",
			"author": baseUser("alice"),
			"body":   "Some comment on Artie's issue.",
		},
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (comment on mayor's issue)", err)
	}
}

func TestRelevance_CommentMentionMayor_ByAccountID_Delivered(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "comment_created",
		"issue":        baseIssue("PROJ-106"), // unassigned
		"comment": map[string]any{
			"id":     "c-2",
			"author": baseUser("alice"),
			"body":   "Hey [~accountid:712020:abcd-1234] — please look.",
		},
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (@-mention by accountId)", err)
	}
}

func TestRelevance_CommentMentionMayor_ByUsername_Delivered(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "comment_created",
		"issue":        baseIssue("PROJ-107"),
		"comment": map[string]any{
			"id":     "c-3",
			"author": baseUser("alice"),
			"body":   "Pinging [~Artie] for visibility.",
		},
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (@-mention by username)", err)
	}
}

func TestRelevance_CommentOnOtherIssue_NoMention_Dropped(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, map[string]any{
		"timestamp":    1713571200000,
		"webhookEvent": "comment_created",
		"issue":        issueWithAssignee("PROJ-108", "", "712020:other", "Other"),
		"comment": map[string]any{
			"id":     "c-4",
			"author": baseUser("alice"),
			"body":   "Unrelated chatter.",
		},
	})
	evt, err := tr.Translate(nil, body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant", err)
	}
	if evt == nil {
		t.Fatal("expected non-nil event for audit log")
	}
}

func TestRelevance_AssigneeUsernameMatch_CaseInsensitive(t *testing.T) {
	t.Parallel()
	tr := jira.New(testSecret, nil, jira.MayorIdentity{Usernames: []string{"artie"}}, nil)
	body := mustMarshal(t, issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:issue_created",
		User:         baseUser("alice"),
		Issue:        issueWithAssignee("PROJ-109", "Artie", "", ""),
	})
	if _, err := tr.Translate(nil, body); err != nil {
		t.Fatalf("Translate: %v, want nil (case-insensitive username match)", err)
	}
}

func TestRelevance_UnknownEventType_NotShadowedByRelevance(t *testing.T) {
	t.Parallel()
	// Make sure ErrUnknownEventType still wins over relevance filtering — the
	// dispatcher uses different drop reasons for the two.
	tr := jira.New(testSecret, nil, mayorJiraIdentity(), nil)
	body := mustMarshal(t, map[string]any{
		"webhookEvent": "jira:sprint_started",
		"issue":        baseIssue("PROJ-110"),
	})
	if _, err := tr.Translate(nil, body); !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("Translate: err = %v, want ErrUnknownEventType", err)
	}
}

// TestMayorIdentity_HasAny_IgnoresEmptyEntries asserts HasAny doesn't return
// true when every entry is empty or whitespace-only. Without this, a caller
// who passes `MayorIdentity{Usernames: []string{""}}` would enable relevance
// filtering with no actual match targets.
func TestMayorIdentity_HasAny_IgnoresEmptyEntries(t *testing.T) {
	if (jira.MayorIdentity{Usernames: []string{""}}).HasAny() {
		t.Error("HasAny() = true for empty username, want false")
	}
	if (jira.MayorIdentity{AccountIDs: []string{"  "}, Usernames: []string{"\t"}}).HasAny() {
		t.Error("HasAny() = true for whitespace-only entries, want false")
	}
	if !(jira.MayorIdentity{Usernames: []string{"", "Artie"}}).HasAny() {
		t.Error("HasAny() = false despite one populated entry, want true")
	}
}

func TestActorFilter_CaseSensitive_MixedCaseNoMatch(t *testing.T) {
	t.Parallel()
	// Filter has lowercase "artie", actor is "Artie" — no match expected.
	tr := jira.New(testSecret, []string{"artie"}, jira.MayorIdentity{}, nil)
	body := mustMarshalAny(issuePayload{
		Timestamp:    1713571200000,
		WebhookEvent: "jira:comment_added",
		Issue:        baseIssue("PROJ-14"),
		Comment: map[string]any{
			"id":      "cmt-4",
			"body":    "hello",
			"created": "2026-04-19T12:00:00.000+0000",
			"author":  map[string]string{"name": "Artie"},
		},
	})
	evt, err := tr.Translate(nil, body)
	if err != nil {
		t.Fatalf("Translate() error = %v, want nil (case mismatch should not filter)", err)
	}
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
}
