package jira

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

// testSecret is the HMAC secret used in all authentication tests.
const testSecret = "test-webhook-secret"

// makeSignature computes the X-Hub-Signature value for the given body.
func makeSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newTestTranslator returns a Translator using the test secret.
func newTestTranslator() *Translator {
	return New([]byte(testSecret), nil)
}

func TestProvider(t *testing.T) {
	if got := newTestTranslator().Provider(); got != "jira" {
		t.Errorf("Provider() = %q, want %q", got, "jira")
	}
}

func TestAuthenticate(t *testing.T) {
	tr := newTestTranslator()
	body := `{"webhookEvent":"jira:issue_created"}`

	tests := []struct {
		name    string
		headers map[string]string
		wantErr bool
	}{
		{
			name:    "valid signature",
			headers: map[string]string{"x-hub-signature": makeSignature(testSecret, body)},
			wantErr: false,
		},
		{
			name:    "missing header",
			headers: map[string]string{},
			wantErr: true,
		},
		{
			name:    "wrong signature",
			headers: map[string]string{"x-hub-signature": "sha256=deadbeef"},
			wantErr: true,
		},
		{
			name:    "invalid hex",
			headers: map[string]string{"x-hub-signature": "sha256=notvalidhex!"},
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			headers: map[string]string{"x-hub-signature": "md5=abc123"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tr.Authenticate(tt.headers, []byte(body))
			if (err != nil) != tt.wantErr {
				t.Errorf("Authenticate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// issuePayload builds a minimal Jira issue webhook payload.
func issuePayload(webhookEvent, issueKey string, extraFields map[string]any) []byte {
	p := map[string]any{
		"id":           int64(99001),
		"timestamp":    int64(1745088000000), // 2025-04-19T20:00:00Z
		"webhookEvent": webhookEvent,
		"user": map[string]any{
			"name":        "alice",
			"displayName": "Alice Smith",
		},
		"issue": map[string]any{
			"id":  "10001",
			"key": issueKey,
			"self": fmt.Sprintf("https://company.atlassian.net/rest/api/2/issue/10001"),
			"fields": map[string]any{
				"summary":     "Fix login timeout",
				"description": "When the user is idle for 30 minutes...",
				"labels":      []string{"backend", "critical"},
				"status":      map[string]any{"name": "In Progress"},
				"priority":    map[string]any{"name": "High"},
			},
		},
	}
	for k, v := range extraFields {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return b
}

// commentPayload builds a Jira comment webhook payload.
func commentPayload(webhookEvent, issueKey string) []byte {
	p := map[string]any{
		"id":           int64(99002),
		"timestamp":    int64(1745088060000),
		"webhookEvent": webhookEvent,
		"user": map[string]any{
			"name":        "bob",
			"displayName": "Bob Builder",
		},
		"issue": map[string]any{
			"id":  "10001",
			"key": issueKey,
			"self": "https://company.atlassian.net/rest/api/2/issue/10001",
			"fields": map[string]any{
				"summary": "Fix login timeout",
			},
		},
		"comment": map[string]any{
			"id": "20001",
			"author": map[string]any{
				"name":        "carol",
				"displayName": "Carol",
			},
			"body": "Looks like the session TTL is set to 30 min by default.",
		},
	}
	b, _ := json.Marshal(p)
	return b
}

func TestTranslate_IssueCreated(t *testing.T) {
	tr := newTestTranslator()
	body := issuePayload("jira:issue_created", "PROJ-1234", nil)

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() unexpected error: %v", err)
	}

	if ev.Provider != "jira" {
		t.Errorf("Provider = %q, want %q", ev.Provider, "jira")
	}
	if ev.EventType != "issue.created" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "issue.created")
	}
	if ev.EventID != "99001" {
		t.Errorf("EventID = %q, want %q", ev.EventID, "99001")
	}
	if ev.Actor != "alice" {
		t.Errorf("Actor = %q, want %q", ev.Actor, "alice")
	}
	if ev.Subject != "PROJ-1234" {
		t.Errorf("Subject = %q, want %q", ev.Subject, "PROJ-1234")
	}
	if ev.CanonicalURL != "https://company.atlassian.net/browse/PROJ-1234" {
		t.Errorf("CanonicalURL = %q, want %q", ev.CanonicalURL, "https://company.atlassian.net/browse/PROJ-1234")
	}
	if ev.Text != "Fix login timeout" {
		t.Errorf("Text = %q, want %q", ev.Text, "Fix login timeout")
	}
	if len(ev.Labels) != 2 {
		t.Errorf("Labels len = %d, want 2", len(ev.Labels))
	}
	wantTS := time.UnixMilli(1745088000000).UTC()
	if !ev.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, wantTS)
	}
}

func TestTranslate_IssueUpdated(t *testing.T) {
	tr := newTestTranslator()
	body := issuePayload("jira:issue_updated", "PROJ-1234", map[string]any{
		"changelog": map[string]any{
			"id": "10011",
			"items": []map[string]any{
				{"field": "status", "fromString": "To Do", "toString": "In Progress"},
				{"field": "priority", "fromString": "Medium", "toString": "High"},
			},
		},
	})

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() unexpected error: %v", err)
	}
	if ev.EventType != "issue.updated" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "issue.updated")
	}
	if !strings.Contains(ev.Text, "status: To Do → In Progress") {
		t.Errorf("Text missing changelog delta: %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "Fix login timeout") {
		t.Errorf("Text missing summary: %q", ev.Text)
	}
}

func TestTranslate_IssueUpdated_NoChangelog(t *testing.T) {
	tr := newTestTranslator()
	body := issuePayload("jira:issue_updated", "PROJ-5678", nil)

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() unexpected error: %v", err)
	}
	if ev.EventType != "issue.updated" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "issue.updated")
	}
	if ev.Text != "Fix login timeout" {
		t.Errorf("Text = %q, want summary fallback", ev.Text)
	}
}

func TestTranslate_CommentAdded(t *testing.T) {
	tr := newTestTranslator()
	body := commentPayload("jira:comment_added", "PROJ-1234")

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() unexpected error: %v", err)
	}
	if ev.EventType != "comment.added" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "comment.added")
	}
	// Actor comes from comment.author, not top-level user
	if ev.Actor != "carol" {
		t.Errorf("Actor = %q, want %q", ev.Actor, "carol")
	}
	if ev.Text != "Looks like the session TTL is set to 30 min by default." {
		t.Errorf("Text = %q, want comment body", ev.Text)
	}
}

func TestTranslate_CommentUpdated(t *testing.T) {
	tr := newTestTranslator()
	body := commentPayload("jira:comment_updated", "PROJ-1234")

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.EventType != "comment.updated" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "comment.updated")
	}
}

func TestTranslate_OldStyleCommentAdded(t *testing.T) {
	tr := newTestTranslator()
	// Older Jira versions use "comment_created" without the "jira:" prefix
	body := commentPayload("comment_created", "PROJ-1234")

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() unexpected error: %v", err)
	}
	if ev.EventType != "comment.added" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "comment.added")
	}
}

func TestTranslate_UnknownEventType(t *testing.T) {
	tr := newTestTranslator()
	body := issuePayload("jira:issue_deleted", "PROJ-9999", nil)

	ev, err := tr.Translate(body)
	if ev != nil {
		t.Errorf("expected nil NormalizedEvent for unknown type, got %+v", ev)
	}
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("error = %v, want ErrUnknownEventType", err)
	}
}

func TestTranslate_ParseError(t *testing.T) {
	tr := newTestTranslator()
	_, err := tr.Translate([]byte("not json {{{"))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "parse_error:") {
		t.Errorf("error = %q, want parse_error: prefix", err.Error())
	}
}

func TestTranslate_CloudAccountID(t *testing.T) {
	// Jira Cloud uses accountId, not name
	p := map[string]any{
		"id":           int64(99003),
		"timestamp":    int64(1745088000000),
		"webhookEvent": "jira:issue_created",
		"user": map[string]any{
			"accountId":   "5a123abc",
			"displayName": "Dave Cloud",
		},
		"issue": map[string]any{
			"id":  "10002",
			"key": "CLOUD-42",
			"self": "https://myorg.atlassian.net/rest/api/2/issue/10002",
			"fields": map[string]any{
				"summary": "Cloud issue",
				"labels":  []string{},
			},
		},
	}
	body, _ := json.Marshal(p)
	tr := newTestTranslator()

	ev, err := tr.Translate(body)
	if err != nil {
		t.Fatalf("Translate() unexpected error: %v", err)
	}
	if ev.Actor != "5a123abc" {
		t.Errorf("Actor = %q, want accountId %q", ev.Actor, "5a123abc")
	}
	if ev.CanonicalURL != "https://myorg.atlassian.net/browse/CLOUD-42" {
		t.Errorf("CanonicalURL = %q", ev.CanonicalURL)
	}
}

func TestCanonicalURL_MissingIssueKey(t *testing.T) {
	p := &jiraPayload{
		Issue: jiraIssue{
			Key:  "",
			Self: "https://company.atlassian.net/rest/api/2/issue/10001",
		},
	}
	if got := p.canonicalURL(); got != "" {
		t.Errorf("canonicalURL() = %q, want empty for missing key", got)
	}
}

func TestCanonicalURL_InvalidSelf(t *testing.T) {
	p := &jiraPayload{
		Issue: jiraIssue{
			Key:  "PROJ-1",
			Self: "not a url ://bad",
		},
	}
	// Should not panic; URL may be empty or scheme-less
	_ = p.canonicalURL()
}
