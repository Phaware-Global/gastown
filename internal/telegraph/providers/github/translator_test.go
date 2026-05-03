package github_test

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
	return github.New(cfg.secret, cfg.events, cfg.ignoreActors, cfg.allowedRepos, nil)
}

type translatorConfig struct {
	secret       string
	events       []string
	ignoreActors []string
	allowedRepos []string
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
		"action": "closed",
		"sender": map[string]any{"login": "alice"},
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
		"action": "opened",
		"sender": map[string]any{"login": "carol"},
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
