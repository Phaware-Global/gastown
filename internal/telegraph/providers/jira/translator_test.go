package jira_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	return jira.New(testSecret, nil)
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
	tr := jira.New("wrong-secret", nil)
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
	evt, err := tr.Translate(body)
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
	_, err := tr.Translate(body)
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
	evt, err := tr.Translate(body)
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
	evt, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.Text != "" {
		t.Errorf("expected empty Text for no changelog, got %q", evt.Text)
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
	evt, err := tr.Translate(body)
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

func TestTranslate_CommentAdded_MissingComment(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"webhookEvent": "jira:comment_added",
		"issue":        baseIssue("PROJ-5"),
	})
	_, err := tr.Translate(body)
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
	evt, err := tr.Translate(body)
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

// ---- unknown event type ----

func TestTranslate_UnknownEventType(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	body := mustMarshal(t, map[string]any{
		"webhookEvent": "jira:sprint_started",
		"issue":        baseIssue("PROJ-7"),
	})
	_, err := tr.Translate(body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("Translate() error = %v, want ErrUnknownEventType", err)
	}
}

// ---- parse errors ----

func TestTranslate_InvalidJSON(t *testing.T) {
	t.Parallel()
	tr := newTranslator()
	_, err := tr.Translate([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Error("parse error should not be ErrUnknownEventType")
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
	evt, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if evt.Labels == nil {
		t.Error("Labels should not be nil — must be empty slice")
	}
}
