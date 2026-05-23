package github_test

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
	github "github.com/steveyegge/gastown/internal/telegraph/providers/github"
)

const testSecret = "github-test-secret-xyzzy"

// ---- helpers ----

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newTranslator(t *testing.T, opts ...translatorOpt) *github.Translator {
	t.Helper()
	cfg := translatorConfig{secret: testSecret}
	for _, o := range opts {
		o(&cfg)
	}
	return github.New(cfg.secret, cfg.events, cfg.ignoreActors, cfg.allowedRepos, github.MayorIdentity{Logins: cfg.mayorLogins}, nil)
}

type translatorConfig struct {
	secret       string
	events       []string
	ignoreActors []string
	allowedRepos []string
	mayorLogins  []string
}

type translatorOpt func(*translatorConfig)

func withEvents(events ...string) translatorOpt {
	return func(c *translatorConfig) { c.events = events }
}

func withIgnoreActors(actors ...string) translatorOpt {
	return func(c *translatorConfig) { c.ignoreActors = actors }
}

func withAllowedRepos(repos ...string) translatorOpt {
	return func(c *translatorConfig) { c.allowedRepos = repos }
}

func withSecret(secret string) translatorOpt {
	return func(c *translatorConfig) { c.secret = secret }
}

func withMayorLogins(logins ...string) translatorOpt {
	return func(c *translatorConfig) { c.mayorLogins = logins }
}

// headersFor returns lowercased headers for a wire event with optional delivery ID.
func headersFor(wireEvent string) map[string]string {
	return map[string]string{
		"x-github-event":    wireEvent,
		"x-github-delivery": "test-delivery-" + wireEvent,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---- Authenticate ----

func TestAuthenticate_Valid(t *testing.T) {
	tr := newTranslator(t)
	body := []byte(`{"action":"opened"}`)
	headers := map[string]string{"x-hub-signature-256": sign(body)}
	if err := tr.Authenticate(headers, body); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestAuthenticate_MissingHeader(t *testing.T) {
	tr := newTranslator(t)
	if err := tr.Authenticate(map[string]string{}, []byte("body")); err == nil {
		t.Fatal("expected error for missing header")
	}
}

func TestAuthenticate_LegacySHA1Rejected(t *testing.T) {
	// The legacy x-hub-signature (SHA-1) header alone must not authenticate;
	// only x-hub-signature-256 is honored.
	tr := newTranslator(t)
	body := []byte(`{}`)
	headers := map[string]string{"x-hub-signature": "sha1=abc123"}
	if err := tr.Authenticate(headers, body); err == nil {
		t.Fatal("expected error when only x-hub-signature (sha1) is present")
	}
}

func TestAuthenticate_WrongSecret(t *testing.T) {
	tr := newTranslator(t, withSecret("a-different-secret"))
	body := []byte(`{"action":"opened"}`)
	headers := map[string]string{"x-hub-signature-256": sign(body)}
	if err := tr.Authenticate(headers, body); err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestAuthenticate_BadPrefix(t *testing.T) {
	tr := newTranslator(t)
	headers := map[string]string{"x-hub-signature-256": "md5=deadbeef"}
	if err := tr.Authenticate(headers, []byte("body")); err == nil {
		t.Fatal("expected error for non-sha256 prefix")
	}
}

func TestAuthenticate_InvalidHex(t *testing.T) {
	tr := newTranslator(t)
	headers := map[string]string{"x-hub-signature-256": "sha256=zzz"}
	if err := tr.Authenticate(headers, []byte("body")); err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

// ---- Translate: missing or unknown wire events ----

func TestTranslate_MissingHeader(t *testing.T) {
	tr := newTranslator(t)
	_, err := tr.Translate(map[string]string{}, []byte(`{}`))
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Fatalf("Translate: err = %v, want ErrUnknownEventType", err)
	}
}

func TestTranslate_UnknownWireEvent(t *testing.T) {
	tr := newTranslator(t)
	_, err := tr.Translate(headersFor("ping"), []byte(`{"zen":"hi"}`))
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Fatalf("Translate: err = %v, want ErrUnknownEventType", err)
	}
}

func TestTranslate_EventClassNotEnabled(t *testing.T) {
	// Operator opted in only to pull_request; a check_run delivery must drop.
	tr := newTranslator(t, withEvents("pull_request"))
	body := mustJSON(t, map[string]any{"action": "completed"})
	_, err := tr.Translate(headersFor("check_run"), body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Fatalf("Translate: err = %v, want ErrUnknownEventType", err)
	}
}

func TestTranslate_BadJSON(t *testing.T) {
	tr := newTranslator(t)
	_, err := tr.Translate(headersFor("pull_request"), []byte(`not json`))
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

// ---- pull_request: merged ----

func prClosedMergedPayload() []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "closed",
		"sender": map[string]any{"login": "alice"},
		"repository": map[string]any{
			"full_name": "acme/widget",
			"html_url":  "https://github.com/acme/widget",
		},
		"pull_request": map[string]any{
			"number":     42,
			"html_url":   "https://github.com/acme/widget/pull/42",
			"title":      "Fix login flow",
			"body":       "Closes #41",
			"merged":     true,
			"merged_at":  "2026-04-29T15:00:00Z",
			"updated_at": "2026-04-29T15:00:00Z",
			"user":       map[string]any{"login": "bob"},
			"labels":     []map[string]any{{"name": "bug"}, {"name": "p1"}},
		},
	})
	return b
}

func TestTranslate_PullRequestMerged(t *testing.T) {
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("pull_request"), prClosedMergedPayload())
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.Provider != "github" {
		t.Errorf("Provider = %q", evt.Provider)
	}
	if evt.EventType != "pull_request.merged" {
		t.Errorf("EventType = %q, want pull_request.merged", evt.EventType)
	}
	if evt.Subject != "acme/widget#42" {
		t.Errorf("Subject = %q", evt.Subject)
	}
	if evt.Actor != "alice" {
		t.Errorf("Actor = %q", evt.Actor)
	}
	if evt.CanonicalURL != "https://github.com/acme/widget/pull/42" {
		t.Errorf("CanonicalURL = %q", evt.CanonicalURL)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-04-29T15:00:00Z")
	if !evt.Timestamp.Equal(wantTime) {
		t.Errorf("Timestamp = %v, want %v", evt.Timestamp, wantTime)
	}
	if !strings.Contains(evt.Text, "Fix login flow") {
		t.Errorf("Text missing PR title: %q", evt.Text)
	}
}

func TestTranslate_PullRequestClosedUnmerged(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "closed",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number":     7,
			"html_url":   "https://github.com/acme/widget/pull/7",
			"title":      "Abandoned",
			"merged":     false,
			"updated_at": "2026-04-29T15:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("pull_request"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "pull_request.closed_unmerged" {
		t.Errorf("EventType = %q, want pull_request.closed_unmerged", evt.EventType)
	}
}

func TestTranslate_PullRequestOpened(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "opened",
		"sender":     map[string]any{"login": "carol"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number":     9,
			"html_url":   "https://github.com/acme/widget/pull/9",
			"title":      "Add feature",
			"merged":     false,
			"updated_at": "2026-04-29T15:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("pull_request"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "pull_request.opened" {
		t.Errorf("EventType = %q", evt.EventType)
	}
}

func TestTranslate_PullRequestActionOutOfScope(t *testing.T) {
	// "labeled" is not in scope — should drop as ErrUnknownEventType.
	body := mustJSON(t, map[string]any{
		"action":       "labeled",
		"repository":   map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{"number": 1, "merged": false, "updated_at": "2026-04-29T15:00:00Z"},
	})
	tr := newTranslator(t)
	_, err := tr.Translate(headersFor("pull_request"), body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("err = %v, want ErrUnknownEventType", err)
	}
}

// ---- pull_request_review ----

func TestTranslate_PullRequestReviewSubmitted(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "submitted",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number":   42,
			"html_url": "https://github.com/acme/widget/pull/42",
			"title":    "Fix login",
			"merged":   false,
		},
		"review": map[string]any{
			"id":           7,
			"html_url":     "https://github.com/acme/widget/pull/42#pullrequestreview-7",
			"state":        "approved",
			"body":         "LGTM",
			"user":         map[string]any{"login": "alice"},
			"submitted_at": "2026-04-29T16:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("pull_request_review"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "pull_request.review_submitted" {
		t.Errorf("EventType = %q", evt.EventType)
	}
	if evt.Subject != "acme/widget#42" {
		t.Errorf("Subject = %q", evt.Subject)
	}
	hasState := false
	for _, l := range evt.Labels {
		if l == "review:approved" {
			hasState = true
		}
	}
	if !hasState {
		t.Errorf("Labels missing review:approved: %v", evt.Labels)
	}
	if evt.Text != "LGTM" {
		t.Errorf("Text = %q, want LGTM", evt.Text)
	}
}

func TestTranslate_PullRequestReviewEditedDropped(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":       "edited",
		"repository":   map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{"number": 1},
		"review":       map[string]any{"id": 1, "state": "approved"},
	})
	tr := newTranslator(t)
	_, err := tr.Translate(headersFor("pull_request_review"), body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("err = %v, want ErrUnknownEventType", err)
	}
}

// ---- pull_request_review_comment (line comment) ----

func TestTranslate_PullRequestReviewComment(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "created",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number":   42,
			"html_url": "https://github.com/acme/widget/pull/42",
			"merged":   false,
		},
		"comment": map[string]any{
			"id":         55,
			"html_url":   "https://github.com/acme/widget/pull/42#discussion_r55",
			"body":       "Please rename this variable",
			"path":       "src/login.go",
			"user":       map[string]any{"login": "alice"},
			"created_at": "2026-04-29T17:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("pull_request_review_comment"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "pull_request.review_comment" {
		t.Errorf("EventType = %q", evt.EventType)
	}
	hasPath := false
	for _, l := range evt.Labels {
		if l == "path:src/login.go" {
			hasPath = true
		}
	}
	if !hasPath {
		t.Errorf("Labels missing path: %v", evt.Labels)
	}
}

// ---- issue_comment (PR-only) ----

func TestTranslate_IssueComment_OnPR(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "created",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"issue": map[string]any{
			"number":   42,
			"html_url": "https://github.com/acme/widget/pull/42",
			"pull_request": map[string]any{
				"html_url": "https://github.com/acme/widget/pull/42",
			},
		},
		"comment": map[string]any{
			"id":         99,
			"html_url":   "https://github.com/acme/widget/pull/42#issuecomment-99",
			"body":       "Looks good",
			"user":       map[string]any{"login": "alice"},
			"created_at": "2026-04-29T18:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("issue_comment"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "pull_request.comment" {
		t.Errorf("EventType = %q", evt.EventType)
	}
}

func TestTranslate_IssueComment_OnNonPR_Dropped(t *testing.T) {
	// Issue comment without issue.pull_request → out of scope.
	body := mustJSON(t, map[string]any{
		"action":     "created",
		"repository": map[string]any{"full_name": "acme/widget"},
		"issue":      map[string]any{"number": 1, "html_url": "https://example/issues/1"},
		"comment":    map[string]any{"id": 1, "body": "hi"},
	})
	tr := newTranslator(t)
	_, err := tr.Translate(headersFor("issue_comment"), body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("err = %v, want ErrUnknownEventType", err)
	}
}

// ---- check_run / check_suite / workflow_run ----

func TestTranslate_CheckRunFailed(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"sender":     map[string]any{"login": "github-actions[bot]"},
		"repository": map[string]any{"full_name": "acme/widget", "html_url": "https://github.com/acme/widget"},
		"check_run": map[string]any{
			"id":           1001,
			"name":         "lint",
			"html_url":     "https://github.com/acme/widget/runs/1001",
			"status":       "completed",
			"conclusion":   "failure",
			"head_sha":     "abc1234567890",
			"completed_at": "2026-04-29T19:00:00Z",
			"pull_requests": []map[string]any{
				{"number": 42, "html_url": "https://github.com/acme/widget/pull/42"},
			},
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("check_run"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "check_run.failed" {
		t.Errorf("EventType = %q", evt.EventType)
	}
	if evt.Subject != "acme/widget#42" {
		t.Errorf("Subject = %q", evt.Subject)
	}
}

func TestTranslate_CheckRunSuccessNotForwarded(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"repository": map[string]any{"full_name": "acme/widget"},
		"check_run":  map[string]any{"id": 1, "conclusion": "success"},
	})
	tr := newTranslator(t)
	_, err := tr.Translate(headersFor("check_run"), body)
	if !errors.Is(err, telegraph.ErrUnknownEventType) {
		t.Errorf("success conclusion: err = %v, want ErrUnknownEventType", err)
	}
}

func TestTranslate_CheckRunFallbackSubjectFromSHA(t *testing.T) {
	// No PR association → subject should fall back to owner/repo@sha7.
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"repository": map[string]any{"full_name": "acme/widget"},
		"check_run": map[string]any{
			"id":         1,
			"conclusion": "failure",
			"head_sha":   "deadbeefcafef00d",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("check_run"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.Subject != "acme/widget@deadbee" {
		t.Errorf("Subject = %q, want acme/widget@deadbee", evt.Subject)
	}
}

func TestTranslate_CheckSuiteFailed(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"repository": map[string]any{"full_name": "acme/widget", "html_url": "https://github.com/acme/widget"},
		"check_suite": map[string]any{
			"id":         42,
			"head_sha":   "deadbee123456",
			"status":     "completed",
			"conclusion": "timed_out",
			"updated_at": "2026-04-29T20:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("check_suite"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "check_suite.failed" {
		t.Errorf("EventType = %q", evt.EventType)
	}
	if !strings.Contains(evt.CanonicalURL, "/commit/deadbee123456") {
		t.Errorf("CanonicalURL = %q", evt.CanonicalURL)
	}
}

func TestTranslate_WorkflowRunFailed(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"repository": map[string]any{"full_name": "acme/widget"},
		"workflow_run": map[string]any{
			"id":          5,
			"name":        "CI",
			"html_url":    "https://github.com/acme/widget/actions/runs/5",
			"head_sha":    "abc1234",
			"head_branch": "main",
			"status":      "completed",
			"conclusion":  "failure",
			"updated_at":  "2026-04-29T21:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("workflow_run"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if evt.EventType != "workflow_run.failed" {
		t.Errorf("EventType = %q", evt.EventType)
	}
}

// ---- Repo filter ----

func TestTranslate_RepoFilterDrops(t *testing.T) {
	tr := newTranslator(t, withAllowedRepos("acme/other"))
	evt, err := tr.Translate(headersFor("pull_request"), prClosedMergedPayload())
	if !errors.Is(err, telegraph.ErrRepoFiltered) {
		t.Fatalf("err = %v, want ErrRepoFiltered", err)
	}
	if evt == nil || evt.Subject != "acme/widget#42" {
		t.Errorf("filtered event lost metadata: %+v", evt)
	}
}

func TestTranslate_RepoFilterAllows(t *testing.T) {
	tr := newTranslator(t, withAllowedRepos("acme/widget"))
	if _, err := tr.Translate(headersFor("pull_request"), prClosedMergedPayload()); err != nil {
		t.Fatalf("Translate: %v", err)
	}
}

func TestTranslate_RepoFilterCaseInsensitive(t *testing.T) {
	tr := newTranslator(t, withAllowedRepos("Acme/Widget"))
	if _, err := tr.Translate(headersFor("pull_request"), prClosedMergedPayload()); err != nil {
		t.Fatalf("Translate: %v", err)
	}
}

func TestTranslate_RepoFilterMissingRepoIsDropped(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":     1,
			"merged":     true,
			"updated_at": "2026-04-29T15:00:00Z",
		},
	})
	tr := newTranslator(t, withAllowedRepos("acme/widget"))
	_, err := tr.Translate(headersFor("pull_request"), body)
	if !errors.Is(err, telegraph.ErrRepoFiltered) {
		t.Fatalf("err = %v, want ErrRepoFiltered", err)
	}
}

// ---- Actor filter ----

func TestTranslate_ActorFilter(t *testing.T) {
	tr := newTranslator(t, withIgnoreActors("alice"))
	evt, err := tr.Translate(headersFor("pull_request"), prClosedMergedPayload())
	if !errors.Is(err, telegraph.ErrActorFiltered) {
		t.Fatalf("err = %v, want ErrActorFiltered", err)
	}
	if evt == nil || evt.Actor != "alice" {
		t.Errorf("filtered event missing actor: %+v", evt)
	}
}

func TestTranslate_ActorFilterCaseSensitive(t *testing.T) {
	tr := newTranslator(t, withIgnoreActors("Alice"))
	if _, err := tr.Translate(headersFor("pull_request"), prClosedMergedPayload()); err != nil {
		t.Fatalf("Translate: %v (case-sensitive filter must NOT match 'alice')", err)
	}
}

// ---- Provider ----

func TestProvider(t *testing.T) {
	tr := newTranslator(t)
	if got := tr.Provider(); got != "github" {
		t.Errorf("Provider = %q, want github", got)
	}
}

// ---- SupportedWireEvents ----

func TestSupportedWireEvents(t *testing.T) {
	want := []string{
		"pull_request",
		"pull_request_review",
		"pull_request_review_comment",
		"issue_comment",
		"check_run",
		"check_suite",
		"workflow_run",
	}
	got := github.SupportedWireEvents
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// ---- timestamp fallback ----

// TestTranslate_MalformedTimestampYieldsZero pins the parseGHTime contract:
// when a timestamp field is missing, JSON null, or unparseable, the resulting
// NormalizedEvent.Timestamp is the zero value rather than time.Now() — masking
// malformed payloads as "events from right now" was the gemini-code-assist
// concern on PR #60. Callers / dashboards can detect the zero explicitly.
func TestTranslate_MalformedTimestampYieldsZero(t *testing.T) {
	cases := []struct {
		name     string
		mergedAt any // any so we can emit JSON null
	}{
		{name: "empty", mergedAt: ""},
		{name: "null", mergedAt: nil},
		{name: "garbage", mergedAt: "not-a-timestamp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"action":     "closed",
				"sender":     map[string]any{"login": "alice"},
				"repository": map[string]any{"full_name": "acme/widget"},
				"pull_request": map[string]any{
					"number":     1,
					"html_url":   "https://github.com/acme/widget/pull/1",
					"title":      "x",
					"merged":     true,
					"merged_at":  tc.mergedAt,
					"updated_at": tc.mergedAt,
				},
			})
			tr := newTranslator(t)
			evt, err := tr.Translate(headersFor("pull_request"), body)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if !evt.Timestamp.IsZero() {
				t.Errorf("Timestamp = %v, want zero (malformed input must not be substituted with time.Now())", evt.Timestamp)
			}
		})
	}
}

// ---- ValidateEvents ----

func TestValidateEvents_Empty(t *testing.T) {
	if err := github.ValidateEvents(nil); err != nil {
		t.Errorf("nil: %v, want nil", err)
	}
	if err := github.ValidateEvents([]string{}); err != nil {
		t.Errorf("[]: %v, want nil", err)
	}
}

func TestValidateEvents_AllSupported(t *testing.T) {
	if err := github.ValidateEvents([]string{"pull_request", "check_run"}); err != nil {
		t.Errorf("supported set: %v", err)
	}
}

func TestValidateEvents_AnyUnsupportedRejected(t *testing.T) {
	// Mix of supported and typo — must fail to prevent the silent-drop footgun.
	err := github.ValidateEvents([]string{"pull_request", "puul_request"})
	if err == nil {
		t.Fatal("expected error for typo'd entry")
	}
	if !strings.Contains(err.Error(), "puul_request") {
		t.Errorf("error should name the bad entry, got: %v", err)
	}
}

func TestValidateEvents_AllTyposRejected(t *testing.T) {
	if err := github.ValidateEvents([]string{"foo", "bar"}); err == nil {
		t.Fatal("expected error for all-typo events list")
	}
}

// ---- Mayor relevance filtering ----

// ghPRPayload builds a pull_request webhook body with fine-grained control
// over involvement-relevant fields. Empty / nil arguments are dropped.
func ghPRPayload(repo string, prNum int, action, sender, prAuthor string, assignees, reviewers []string, body, mergedAt string, merged bool) []byte {
	pr := map[string]any{
		"number":     prNum,
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", repo, prNum),
		"title":      "test",
		"body":       body,
		"merged":     merged,
		"merged_at":  mergedAt,
		"updated_at": "2026-04-29T15:00:00Z",
	}
	if prAuthor != "" {
		pr["user"] = map[string]any{"login": prAuthor}
	}
	if len(assignees) > 0 {
		as := make([]map[string]any, 0, len(assignees))
		for _, a := range assignees {
			as = append(as, map[string]any{"login": a})
		}
		pr["assignees"] = as
	}
	if len(reviewers) > 0 {
		rs := make([]map[string]any, 0, len(reviewers))
		for _, r := range reviewers {
			rs = append(rs, map[string]any{"login": r})
		}
		pr["requested_reviewers"] = rs
	}
	out := map[string]any{
		"action":       action,
		"sender":       map[string]any{"login": sender},
		"repository":   map[string]any{"full_name": repo, "html_url": "https://github.com/" + repo},
		"pull_request": pr,
	}
	b, _ := json.Marshal(out)
	return b
}

func TestRelevance_GitHub_PRAuthorIsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := ghPRPayload("acme/widget", 1, "closed", "bot", "Artie",
		nil, nil, "", "2026-04-29T15:00:00Z", true)
	if _, err := tr.Translate(headersFor("pull_request"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (PR author is mayor)", err)
	}
}

func TestRelevance_GitHub_PRAssigneeIsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := ghPRPayload("acme/widget", 2, "closed", "bot", "alice",
		[]string{"artie"}, nil, "", "2026-04-29T15:00:00Z", true)
	if _, err := tr.Translate(headersFor("pull_request"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (mayor is assignee)", err)
	}
}

func TestRelevance_GitHub_PRRequestedReviewerIsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := ghPRPayload("acme/widget", 3, "opened", "alice", "alice",
		nil, []string{"artie"}, "", "2026-04-29T15:00:00Z", false)
	if _, err := tr.Translate(headersFor("pull_request"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (mayor is requested reviewer)", err)
	}
}

func TestRelevance_GitHub_PRSenderIsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := ghPRPayload("acme/widget", 4, "reopened", "Artie", "bob",
		nil, nil, "", "", false)
	if _, err := tr.Translate(headersFor("pull_request"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (mayor triggered the event)", err)
	}
}

func TestRelevance_GitHub_PRBodyMentionsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := ghPRPayload("acme/widget", 5, "opened", "alice", "alice",
		nil, nil, "cc @artie for review", "", false)
	if _, err := tr.Translate(headersFor("pull_request"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (PR body mentions mayor)", err)
	}
}

func TestRelevance_GitHub_PRNoMayorInvolvement_Dropped(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := ghPRPayload("acme/widget", 6, "closed", "bob", "alice",
		[]string{"carol"}, []string{"dave"}, "no mention here",
		"2026-04-29T15:00:00Z", true)
	evt, err := tr.Translate(headersFor("pull_request"), body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant", err)
	}
	if evt == nil || evt.Subject != "acme/widget#6" {
		t.Errorf("ErrNotRelevant must keep audit context: %+v", evt)
	}
}

func TestRelevance_GitHub_IssueCommentOnPR_MentionsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := mustJSON(t, map[string]any{
		"action":     "created",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"issue": map[string]any{
			"number":       7,
			"html_url":     "https://github.com/acme/widget/pull/7",
			"pull_request": map[string]any{"html_url": "https://github.com/acme/widget/pull/7"},
		},
		"comment": map[string]any{
			"id":         100,
			"body":       "Please review, @artie",
			"user":       map[string]any{"login": "alice"},
			"created_at": "2026-04-29T18:00:00Z",
		},
	})
	if _, err := tr.Translate(headersFor("issue_comment"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (comment mentions mayor)", err)
	}
}

func TestRelevance_GitHub_IssueCommentOnPR_OtherAuthor_NoMention_Dropped(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := mustJSON(t, map[string]any{
		"action":     "created",
		"sender":     map[string]any{"login": "bob"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"issue": map[string]any{
			"number":       8,
			"html_url":     "https://github.com/acme/widget/pull/8",
			"pull_request": map[string]any{"html_url": "https://github.com/acme/widget/pull/8"},
		},
		"comment": map[string]any{
			"id":         101,
			"body":       "Just a normal comment",
			"user":       map[string]any{"login": "bob"},
			"created_at": "2026-04-29T18:00:00Z",
		},
	})
	_, err := tr.Translate(headersFor("issue_comment"), body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant", err)
	}
}

func TestRelevance_GitHub_ReviewCommentMentionsMayor_Delivered(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := mustJSON(t, map[string]any{
		"action":     "submitted",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number":   9,
			"html_url": "https://github.com/acme/widget/pull/9",
			"user":     map[string]any{"login": "alice"}, // not mayor
		},
		"review": map[string]any{
			"id":           7,
			"state":        "commented",
			"body":         "fyi @artie",
			"user":         map[string]any{"login": "alice"},
			"submitted_at": "2026-04-29T19:00:00Z",
		},
	})
	if _, err := tr.Translate(headersFor("pull_request_review"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (review body mentions mayor)", err)
	}
}

func TestRelevance_GitHub_CheckRunOnPR_Allowed(t *testing.T) {
	// CI events with PR association pass relevance — best-effort, per code comment.
	tr := newTranslator(t, withMayorLogins("artie"))
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"sender":     map[string]any{"login": "github-actions[bot]"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"check_run": map[string]any{
			"id":         1001,
			"name":       "lint",
			"conclusion": "failure",
			"head_sha":   "abc1234",
			"pull_requests": []map[string]any{
				{"number": 42, "html_url": "https://github.com/acme/widget/pull/42"},
			},
		},
	})
	if _, err := tr.Translate(headersFor("check_run"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (CI tied to a PR is allowed)", err)
	}
}

func TestRelevance_GitHub_CheckRunNoPR_Dropped(t *testing.T) {
	// CI events with no PR association cannot be tied to mayor and are dropped.
	tr := newTranslator(t, withMayorLogins("artie"))
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"sender":     map[string]any{"login": "github-actions[bot]"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"check_run": map[string]any{
			"id":         1002,
			"name":       "scheduled-scan",
			"conclusion": "failure",
			"head_sha":   "deadbee",
		},
	})
	_, err := tr.Translate(headersFor("check_run"), body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant (no PR association)", err)
	}
}

func TestRelevance_GitHub_WorkflowRunNoPR_Dropped(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	body := mustJSON(t, map[string]any{
		"action":     "completed",
		"sender":     map[string]any{"login": "github-actions[bot]"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"workflow_run": map[string]any{
			"id":          5,
			"name":        "CI",
			"conclusion":  "failure",
			"head_branch": "main",
			"updated_at":  "2026-04-29T21:00:00Z",
		},
	})
	_, err := tr.Translate(headersFor("workflow_run"), body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant (no PR)", err)
	}
}

func TestRelevance_GitHub_EmptyIdentity_NoFiltering(t *testing.T) {
	// Backward-compat: empty mayor identity disables relevance filtering.
	tr := newTranslator(t) // no withMayorLogins
	body := ghPRPayload("acme/widget", 99, "closed", "bob", "alice",
		nil, nil, "", "2026-04-29T15:00:00Z", true)
	if _, err := tr.Translate(headersFor("pull_request"), body); err != nil {
		t.Fatalf("Translate: %v, want nil (no mayor identity → all pass)", err)
	}
}

// TestRelevance_GitHub_MentionBoundary checks the mention regex doesn't match
// inside email addresses or other non-mention contexts.
func TestRelevance_GitHub_MentionBoundary(t *testing.T) {
	tr := newTranslator(t, withMayorLogins("artie"))
	// "artie" appears in an email — must NOT count as a mention.
	body := ghPRPayload("acme/widget", 10, "opened", "alice", "alice",
		nil, nil, "Email me at foo+artie@example.com", "", false)
	_, err := tr.Translate(headersFor("pull_request"), body)
	if !errors.Is(err, telegraph.ErrNotRelevant) {
		t.Fatalf("Translate: err = %v, want ErrNotRelevant (no @ mention, only substring)", err)
	}
}

// TestTranslate_ValidTimestampParsed confirms the happy path still parses.
func TestTranslate_ValidTimestampParsed(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"action":     "closed",
		"sender":     map[string]any{"login": "alice"},
		"repository": map[string]any{"full_name": "acme/widget"},
		"pull_request": map[string]any{
			"number":     1,
			"html_url":   "https://github.com/acme/widget/pull/1",
			"title":      "x",
			"merged":     true,
			"merged_at":  "2026-04-29T15:00:00Z",
			"updated_at": "2026-04-29T15:00:00Z",
		},
	})
	tr := newTranslator(t)
	evt, err := tr.Translate(headersFor("pull_request"), body)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-04-29T15:00:00Z")
	if !evt.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", evt.Timestamp, want)
	}
}
