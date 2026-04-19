package jira

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/telegraph"
)

const testSecret = "test-hmac-secret"

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func headers(body []byte) map[string]string {
	return map[string]string{
		"x-hub-signature": signBody(testSecret, body),
		"content-type":    "application/json",
	}
}

func newTranslator() *Translator { return New(testSecret) }

// --- Authenticate tests ---

func TestAuthenticate_valid(t *testing.T) {
	body := []byte(`{"webhookEvent":"jira:issue_created"}`)
	if err := newTranslator().Authenticate(headers(body), body); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestAuthenticate_missingHeader(t *testing.T) {
	body := []byte(`{"webhookEvent":"jira:issue_created"}`)
	err := newTranslator().Authenticate(map[string]string{}, body)
	if err == nil {
		t.Fatal("expected error for missing header")
	}
}

func TestAuthenticate_badSignature(t *testing.T) {
	body := []byte(`{"webhookEvent":"jira:issue_created"}`)
	hdrs := map[string]string{"x-hub-signature": "sha256=deadbeef"}
	err := newTranslator().Authenticate(hdrs, body)
	if err == nil {
		t.Fatal("expected error for bad signature")
	}
}

func TestAuthenticate_wrongSecret(t *testing.T) {
	body := []byte(`{"webhookEvent":"jira:issue_created"}`)
	hdrs := map[string]string{"x-hub-signature": signBody("wrong-secret", body)}
	err := newTranslator().Authenticate(hdrs, body)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestAuthenticate_badHexInSignature(t *testing.T) {
	body := []byte(`{}`)
	hdrs := map[string]string{"x-hub-signature": "sha256=notvalidhex!!!"}
	err := newTranslator().Authenticate(hdrs, body)
	if err == nil {
		t.Fatal("expected error for non-hex signature")
	}
}

func TestAuthenticate_missingPrefix(t *testing.T) {
	body := []byte(`{}`)
	hdrs := map[string]string{"x-hub-signature": "md5=abc"}
	err := newTranslator().Authenticate(hdrs, body)
	if err == nil {
		t.Fatal("expected error for missing sha256= prefix")
	}
}

// --- Translate tests ---

func issuePayload(webhookEvent, key, summary string, extraFields map[string]any) []byte {
	fields := map[string]any{
		"summary":     summary,
		"description": "Some description text.",
		"labels":      []string{"bug", "ui"},
	}
	payload := map[string]any{
		"id":           42,
		"timestamp":    time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC).UnixMilli(),
		"webhookEvent": webhookEvent,
		"user": map[string]any{
			"accountId":    "user-001",
			"displayName":  "Alice",
			"emailAddress": "alice@example.com",
		},
		"issue": map[string]any{
			"id":     "10001",
			"key":    key,
			"self":   "https://company.atlassian.net/rest/api/2/issue/10001",
			"fields": fields,
		},
	}
	for k, v := range extraFields {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	return b
}

func commentPayload(webhookEvent, key string) []byte {
	return issuePayload(webhookEvent, key, "Fix login bug", map[string]any{
		"comment": map[string]any{
			"id":      "20001",
			"author":  map[string]any{"accountId": "user-002", "displayName": "Bob", "emailAddress": "bob@example.com"},
			"body":    "Looks good to me.",
			"created": "2026-04-19T12:00:00.000+0000",
			"updated": "2026-04-19T12:00:00.000+0000",
		},
	})
}

func TestTranslate_issueCreated(t *testing.T) {
	body := issuePayload("jira:issue_created", "PROJ-1234", "Fix login timeout", nil)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.EventType != "issue.created" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "issue.created")
	}
	if ev.Subject != "PROJ-1234" {
		t.Errorf("Subject = %q, want PROJ-1234", ev.Subject)
	}
	if ev.Actor != "alice@example.com" {
		t.Errorf("Actor = %q, want alice@example.com", ev.Actor)
	}
	if ev.Provider != "jira" {
		t.Errorf("Provider = %q, want jira", ev.Provider)
	}
	if ev.CanonicalURL != "https://company.atlassian.net/browse/PROJ-1234" {
		t.Errorf("CanonicalURL = %q", ev.CanonicalURL)
	}
	if ev.EventID != "42" {
		t.Errorf("EventID = %q, want 42", ev.EventID)
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if len(ev.Labels) != 2 {
		t.Errorf("Labels = %v, want [bug ui]", ev.Labels)
	}
}

func TestTranslate_issueUpdated(t *testing.T) {
	body := issuePayload("jira:issue_updated", "PROJ-5678", "Update auth flow", nil)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.EventType != "issue.updated" {
		t.Errorf("EventType = %q, want issue.updated", ev.EventType)
	}
	if ev.Subject != "PROJ-5678" {
		t.Errorf("Subject = %q, want PROJ-5678", ev.Subject)
	}
}

func TestTranslate_commentAdded(t *testing.T) {
	body := commentPayload("jira:comment_added", "PROJ-1234")
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.EventType != "comment.added" {
		t.Errorf("EventType = %q, want comment.added", ev.EventType)
	}
	if ev.Text != "Looks good to me." {
		t.Errorf("Text = %q, want comment body", ev.Text)
	}
	// Actor comes from webhook User, not comment Author
	if ev.Actor != "alice@example.com" {
		t.Errorf("Actor = %q, want alice@example.com", ev.Actor)
	}
}

func TestTranslate_commentUpdated(t *testing.T) {
	body := commentPayload("jira:comment_updated", "PROJ-1234")
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.EventType != "comment.updated" {
		t.Errorf("EventType = %q, want comment.updated", ev.EventType)
	}
}

func TestTranslate_unknownEventType(t *testing.T) {
	body := issuePayload("jira:issue_deleted", "PROJ-1", "Old issue", nil)
	_, err := newTranslator().Translate(body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Fatalf("expected ErrUnknownEventType, got %v", err)
	}
}

func TestTranslate_malformedJSON(t *testing.T) {
	_, err := newTranslator().Translate([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Fatal("malformed JSON should not return ErrUnknownEventType")
	}
}

func TestTranslate_textContainsSummary(t *testing.T) {
	body := issuePayload("jira:issue_created", "PROJ-9", "My summary", nil)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ev.Text, "My summary") {
		t.Errorf("Text does not contain summary: %q", ev.Text)
	}
}

func TestTranslate_descriptionTruncated(t *testing.T) {
	longDesc := string(make([]byte, 300))
	for i := range longDesc {
		longDesc = longDesc[:i] + "x" + longDesc[i+1:]
	}
	payload := map[string]any{
		"id":           1,
		"timestamp":    time.Now().UnixMilli(),
		"webhookEvent": "jira:issue_created",
		"user":         map[string]any{"accountId": "u1", "displayName": "U", "emailAddress": "u@e.com"},
		"issue": map[string]any{
			"id":  "1",
			"key": "PROJ-1",
			"self": "https://x.atlassian.net/rest/api/2/issue/1",
			"fields": map[string]any{
				"summary":     "Short",
				"description": longDesc,
				"labels":      []string{},
			},
		},
	}
	body, _ := json.Marshal(payload)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatal(err)
	}
	// Text should be truncated; total should be less than summary + 200 chars of desc + overhead
	if len(ev.Text) > 250 {
		t.Errorf("text not truncated: len=%d", len(ev.Text))
	}
}

func TestTranslate_timestampUTC(t *testing.T) {
	ms := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC).UnixMilli()
	payload := map[string]any{
		"id":           7,
		"timestamp":    ms,
		"webhookEvent": "jira:issue_created",
		"user":         map[string]any{"accountId": "u1"},
		"issue":        map[string]any{"id": "1", "key": "X-1", "self": "", "fields": map[string]any{"summary": "s", "description": "", "labels": []string{}}},
	}
	body, _ := json.Marshal(payload)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	if !ev.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, want)
	}
	if ev.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp not in UTC: %v", ev.Timestamp.Location())
	}
}

func TestTranslate_actorFallback(t *testing.T) {
	// No email, has displayName
	payload := map[string]any{
		"id":           9,
		"timestamp":    time.Now().UnixMilli(),
		"webhookEvent": "jira:issue_created",
		"user":         map[string]any{"accountId": "uid-999", "displayName": "Charlie", "emailAddress": ""},
		"issue":        map[string]any{"id": "1", "key": "X-1", "self": "", "fields": map[string]any{"summary": "s", "description": "", "labels": []string{}}},
	}
	body, _ := json.Marshal(payload)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actor != "Charlie" {
		t.Errorf("Actor = %q, want Charlie", ev.Actor)
	}

	// No email, no displayName — falls back to accountId
	payload["user"] = map[string]any{"accountId": "uid-999", "displayName": "", "emailAddress": ""}
	body, _ = json.Marshal(payload)
	ev, err = newTranslator().Translate(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actor != "uid-999" {
		t.Errorf("Actor = %q, want uid-999", ev.Actor)
	}
}

func TestTranslate_canonicalURL(t *testing.T) {
	body := issuePayload("jira:issue_created", "PROJ-42", "Title", nil)
	ev, err := newTranslator().Translate(body)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://company.atlassian.net/browse/PROJ-42"
	if ev.CanonicalURL != want {
		t.Errorf("CanonicalURL = %q, want %q", ev.CanonicalURL, want)
	}
}

func TestProvider(t *testing.T) {
	if newTranslator().Provider() != "jira" {
		t.Error("Provider() should return \"jira\"")
	}
}

func TestTranslatorImplementsInterface(t *testing.T) {
	var _ telegraph.Translator = (*Translator)(nil)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
