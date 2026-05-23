// Package github implements the Telegraph L2 Translator for GitHub webhooks.
//
// Scope (v1): notify Mayor on PR comments/reviews, PR merges, and failing
// CI checks. Other GitHub event categories return ErrUnknownEventType.
//
// Auth: HMAC-SHA256 over the raw request body, signed with the operator-managed
// secret. GitHub sends the digest in X-Hub-Signature-256 (e.g.
// "sha256=<hex>"). The legacy SHA-1 X-Hub-Signature header is ignored —
// requiring the SHA-256 form is GitHub's recommended posture.
//
// Event-type detection: GitHub puts the wire-format event class in the
// X-GitHub-Event header (e.g. "pull_request_review"); the action sub-type
// lives in the JSON body's "action" field. The translator combines those
// two values to choose a normalized event type.
//
// Payload parsing: webhook bodies are decoded by github.com/google/go-github
// — its typed event/struct definitions are the canonical schema and pick up
// new GitHub fields automatically. We retain our own HMAC verification
// rather than swapping to github.ValidateSignature; the indirection saves
// nothing while requiring our test fixtures to match the SDK's expectations.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v68/github"
	"github.com/steveyegge/gastown/internal/telegraph"
)

// MayorIdentity identifies the mayor user for GitHub relevance filtering.
// Logins are matched case-insensitively (GitHub logins are case-preserving
// but case-insensitive in comparison).
//
// An empty identity disables relevance filtering — every successfully
// translated event is forwarded. This is the test-only and library-caller
// seam; production deployments go through telegraph.Config.Validate(),
// which refuses an empty identity when the github provider is enabled.
type MayorIdentity struct {
	Logins []string
}

// HasAny reports whether the identity has at least one usable match target.
// Whitespace-only / empty entries are ignored so a slice like `[]string{""}`
// — which the construction-time normalization would discard — does not flip
// HasAny on and enable relevance filtering without any actual match
// targets behind it.
func (m MayorIdentity) HasAny() bool {
	for _, s := range m.Logins {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// Translator implements telegraph.Translator for GitHub webhook payloads.
type Translator struct {
	secret       []byte
	allowedEvent map[string]struct{} // wire-format X-GitHub-Event names operator opted in to
	ignoreActors map[string]struct{}
	allowedRepos map[string]struct{} // case-folded "owner/repo"; nil = no filter
	mayor        MayorIdentity
	mayorLC      map[string]struct{} // case-folded mayor logins
	logger       *slog.Logger
}

// SupportedWireEvents lists the X-GitHub-Event header values this translator
// understands. Values not in this set produce ErrUnknownEventType regardless
// of operator config — the operator may only opt in to events the translator
// can actually translate. Exported so config validation and docs can stay in
// sync with the implementation.
var SupportedWireEvents = []string{
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
	"issue_comment",
	"check_run",
	"check_suite",
	"workflow_run",
}

// ValidateEvents returns an error if any entry in events is not a recognised
// X-GitHub-Event wire-format name. Empty events is valid and means "accept
// every supported event."
//
// Callers (the daemon startup path) should invoke this before constructing a
// Translator so an all-typo events list fails fast at startup instead of
// silently dropping every webhook delivery as ErrUnknownEventType at runtime.
func ValidateEvents(events []string) error {
	if len(events) == 0 {
		return nil
	}
	supported := make(map[string]struct{}, len(SupportedWireEvents))
	for _, e := range SupportedWireEvents {
		supported[e] = struct{}{}
	}
	var unknown []string
	for _, e := range events {
		if _, ok := supported[e]; !ok {
			unknown = append(unknown, e)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("github: unsupported event(s) %v — must be one of %v",
			unknown, SupportedWireEvents)
	}
	return nil
}

// New creates a GitHub Translator.
//
// secret is the HMAC-SHA256 key registered with GitHub's webhook config.
// events is the operator's opt-in list of X-GitHub-Event wire-format names
//
//	(e.g. "pull_request"); empty means "accept every supported event".
//	Names not in SupportedWireEvents are silently ignored.
//
// ignoreActors / allowedRepos are filter sets; nil/empty disables each filter.
//
//	allowedRepos entries are folded to lower-case at construction time.
//
// mayor is the mayor user identity used by relevance filtering. An empty
//
//	identity disables relevance filtering.
//
// logger may be nil; slog.Default() is used if so.
func New(secret string, events []string, ignoreActors []string, allowedRepos []string, mayor MayorIdentity, logger *slog.Logger) *Translator {
	if logger == nil {
		logger = slog.Default()
	}

	supported := make(map[string]struct{}, len(SupportedWireEvents))
	for _, e := range SupportedWireEvents {
		supported[e] = struct{}{}
	}

	var allowedEvent map[string]struct{}
	if len(events) > 0 {
		allowedEvent = make(map[string]struct{}, len(events))
		for _, e := range events {
			if _, ok := supported[e]; ok {
				allowedEvent[e] = struct{}{}
			}
		}
	}

	actorSet := make(map[string]struct{}, len(ignoreActors))
	for _, a := range ignoreActors {
		actorSet[a] = struct{}{}
	}

	var repoSet map[string]struct{}
	if len(allowedRepos) > 0 {
		repoSet = make(map[string]struct{}, len(allowedRepos))
		for _, r := range allowedRepos {
			repoSet[strings.ToLower(r)] = struct{}{}
		}
	}

	mayorLC := make(map[string]struct{}, len(mayor.Logins))
	for _, l := range mayor.Logins {
		if l != "" {
			mayorLC[strings.ToLower(l)] = struct{}{}
		}
	}

	return &Translator{
		secret:       []byte(secret),
		allowedEvent: allowedEvent,
		ignoreActors: actorSet,
		allowedRepos: repoSet,
		mayor:        mayor,
		mayorLC:      mayorLC,
		logger:       logger,
	}
}

// Provider returns the stable provider identifier.
func (t *Translator) Provider() string { return "github" }

// Authenticate verifies the HMAC-SHA256 signature in X-Hub-Signature-256.
// Headers must already be lowercased (L1 contract).
func (t *Translator) Authenticate(headers map[string]string, body []byte) error {
	sig, ok := headers["x-hub-signature-256"]
	if !ok {
		return errors.New("missing x-hub-signature-256 header")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return errors.New("x-hub-signature-256: expected sha256= prefix")
	}
	expected, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return fmt.Errorf("x-hub-signature-256: invalid hex: %w", err)
	}
	mac := hmac.New(sha256.New, t.secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), expected) {
		return errors.New("x-hub-signature-256: HMAC mismatch")
	}
	return nil
}

// Translate converts a GitHub webhook into a NormalizedEvent.
//
// The X-GitHub-Event header selects the translator branch; unknown or
// operator-deselected wire events return ErrUnknownEventType.
//
// Returns (non-nil event, ErrRepoFiltered) when the repository is not in the
// allow-list; (non-nil event, ErrActorFiltered) when the actor is in the
// ignore list; (non-nil event, ErrNotRelevant) when the event does not
// pertain to the mayor. All three are silent drops at L3 with audit
// logging at the dispatcher.
func (t *Translator) Translate(headers map[string]string, body []byte) (*telegraph.NormalizedEvent, error) {
	wireEvent := headers["x-github-event"]
	deliveryID := headers["x-github-delivery"]

	if wireEvent == "" {
		t.logger.Info("telegraph: github webhook missing x-github-event header",
			"delivery_id", deliveryID)
		return nil, telegraph.ErrUnknownEventType
	}

	if t.allowedEvent != nil {
		if _, ok := t.allowedEvent[wireEvent]; !ok {
			t.logger.Info("telegraph: github event class not enabled by config",
				"wire_event", wireEvent, "delivery_id", deliveryID)
			return nil, telegraph.ErrUnknownEventType
		}
	}

	// Pre-gate on SupportedWireEvents so a wire event we don't translate
	// (ping, future GitHub event types, typos) returns ErrUnknownEventType
	// rather than a translate_error from the SDK's "unknown X-Github-Event"
	// error. The allowedEvent check above only fires when the operator
	// supplied an events list — without it, ParseWebHook would otherwise be
	// the first gate and would log noise on harmless deliveries like ping.
	if !isSupportedWireEvent(wireEvent) {
		t.logger.Info("telegraph: unknown github wire event",
			"wire_event", wireEvent, "delivery_id", deliveryID)
		return nil, telegraph.ErrUnknownEventType
	}

	parsed, err := gogithub.ParseWebHook(wireEvent, body)
	if err != nil {
		return nil, fmt.Errorf("github: parsing webhook payload: %w", err)
	}

	var (
		evt    *telegraph.NormalizedEvent
		action string
	)
	switch e := parsed.(type) {
	case *gogithub.PullRequestEvent:
		action = e.GetAction()
		evt, err = translatePullRequest(e, deliveryID)
	case *gogithub.PullRequestReviewEvent:
		action = e.GetAction()
		evt, err = translatePullRequestReview(e, deliveryID)
	case *gogithub.PullRequestReviewCommentEvent:
		action = e.GetAction()
		evt, err = translatePullRequestReviewComment(e, deliveryID)
	case *gogithub.IssueCommentEvent:
		action = e.GetAction()
		evt, err = translateIssueComment(e, deliveryID)
	case *gogithub.CheckRunEvent:
		action = e.GetAction()
		evt, err = translateCheckRun(e, deliveryID)
	case *gogithub.CheckSuiteEvent:
		action = e.GetAction()
		evt, err = translateCheckSuite(e, deliveryID)
	case *gogithub.WorkflowRunEvent:
		action = e.GetAction()
		evt, err = translateWorkflowRun(e, deliveryID)
	default:
		t.logger.Info("telegraph: unknown github wire event",
			"wire_event", wireEvent, "delivery_id", deliveryID)
		return nil, telegraph.ErrUnknownEventType
	}

	if errors.Is(err, telegraph.ErrUnknownEventType) {
		t.logger.Info("telegraph: github action not in scope",
			"wire_event", wireEvent, "action", action, "delivery_id", deliveryID)
		return nil, telegraph.ErrUnknownEventType
	}
	if err != nil {
		return nil, err
	}

	// Repo filter: GitHub repository.full_name is the canonical "owner/repo".
	if t.allowedRepos != nil {
		repoName := strings.ToLower(eventRepoFullName(parsed))
		if repoName == "" {
			// No repo on the payload — treat as filtered (we cannot enforce the
			// allow-list, so default to drop rather than leak).
			return evt, telegraph.ErrRepoFiltered
		}
		if _, ok := t.allowedRepos[repoName]; !ok {
			return evt, telegraph.ErrRepoFiltered
		}
	}

	// Actor filter: drop events whose actor matches an operator-supplied entry.
	// CI-class events (check_run, check_suite, workflow_run) are exempt — their
	// actor is the user who triggered the workflow (typically the committer),
	// but the event itself reports an automated outcome, not an action that
	// user took. Filtering them would suppress CI-failure notifications about
	// the agent's own PRs, which Mayor still wants to see.
	if len(t.ignoreActors) > 0 && !isCIWireEvent(wireEvent) {
		if _, filtered := t.ignoreActors[evt.Actor]; filtered {
			return evt, telegraph.ErrActorFiltered
		}
	}

	// Relevance filter: drop events that do not pertain to mayor. Only applied
	// when a mayor identity is configured; an empty identity disables this
	// stage so tests / library callers behave predictably without it.
	if t.mayor.HasAny() && !t.isRelevant(parsed) {
		return evt, telegraph.ErrNotRelevant
	}

	return evt, nil
}

// isSupportedWireEvent reports whether wireEvent is one this translator
// understands. Used to gate ParseWebHook so unknown/future GitHub events
// route through ErrUnknownEventType rather than the SDK's parse error.
func isSupportedWireEvent(wireEvent string) bool {
	for _, e := range SupportedWireEvents {
		if e == wireEvent {
			return true
		}
	}
	return false
}

// isCIWireEvent reports whether wireEvent reports an automated outcome rather
// than a user-initiated action. The actor filter is bypassed for these so
// CI failures on the agent's own commits still notify Mayor.
func isCIWireEvent(wireEvent string) bool {
	switch wireEvent {
	case "check_run", "check_suite", "workflow_run":
		return true
	}
	return false
}

// isRelevant reports whether the parsed GitHub event pertains to mayor.
//
// Involvement is established from any of:
//   - PR author (pull_request.user.login) == mayor
//   - mayor in pull_request.assignees
//   - mayor in pull_request.requested_reviewers
//   - mayor is the sender / actor of the event (sender.login or comment author)
//   - PR body @-mentions mayor
//   - the triggering comment / review body @-mentions mayor
//   - issue_comment on a PR where mayor is author or assignee
//
// For check_run, check_suite, and workflow_run, GitHub does not carry the PR
// author or reviewer set on the payload — only a PR stub with the number.
// Without state-tracking we cannot verify mayor PR involvement from the
// payload alone, so we apply the documented conservative rule:
//
//   - CI events with no PR association at all (push-to-branch, scheduled
//     workflows) are dropped. There is no PR-author signal to filter on, so
//     forwarding them creates org-wide CI noise.
//   - CI events that *do* have a PR association are allowed through, on the
//     trust boundary established by the operator's repo allow-list. This is
//     a best-effort filter — mayor may still see CI noise on collaborators'
//     PRs in the same repo. A stricter rule would require state, deferred.
func (t *Translator) isRelevant(parsed interface{}) bool {
	switch e := parsed.(type) {
	case *gogithub.CheckRunEvent:
		return e.GetCheckRun() != nil && len(e.GetCheckRun().PullRequests) > 0
	case *gogithub.CheckSuiteEvent:
		return e.GetCheckSuite() != nil && len(e.GetCheckSuite().PullRequests) > 0
	case *gogithub.WorkflowRunEvent:
		return e.GetWorkflowRun() != nil && len(e.GetWorkflowRun().PullRequests) > 0

	case *gogithub.PullRequestEvent:
		return t.matchesLogin(e.GetSender().GetLogin()) ||
			t.prInvolvesMayor(e.GetPullRequest())

	case *gogithub.PullRequestReviewEvent:
		if t.matchesLogin(e.GetSender().GetLogin()) {
			return true
		}
		if t.matchesLogin(e.GetReview().GetUser().GetLogin()) {
			return true
		}
		if t.prInvolvesMayor(e.GetPullRequest()) {
			return true
		}
		return t.bodyMentionsMayor(e.GetReview().GetBody())

	case *gogithub.PullRequestReviewCommentEvent:
		if t.matchesLogin(e.GetSender().GetLogin()) {
			return true
		}
		if t.matchesLogin(e.GetComment().GetUser().GetLogin()) {
			return true
		}
		if t.prInvolvesMayor(e.GetPullRequest()) {
			return true
		}
		return t.bodyMentionsMayor(e.GetComment().GetBody())

	case *gogithub.IssueCommentEvent:
		if t.matchesLogin(e.GetSender().GetLogin()) {
			return true
		}
		if t.matchesLogin(e.GetComment().GetUser().GetLogin()) {
			return true
		}
		// issue_comment on a PR carries the PR author in issue.user, PR
		// assignees in issue.assignees, and the PR description in issue.body
		// (GitHub's webhook never inlines the full pull_request object on
		// issue_comment deliveries). Check all three to stay consistent with
		// PullRequestEvent's prInvolvesMayor coverage.
		if iss := e.GetIssue(); iss != nil && iss.IsPullRequest() {
			if t.matchesLogin(iss.GetUser().GetLogin()) {
				return true
			}
			for _, u := range iss.Assignees {
				if t.matchesLogin(u.GetLogin()) {
					return true
				}
			}
			if t.bodyMentionsMayor(iss.GetBody()) {
				return true
			}
		}
		return t.bodyMentionsMayor(e.GetComment().GetBody())
	}
	return false
}

// prInvolvesMayor reports whether mayor is the author, an assignee, a
// requested reviewer, or @-mentioned in the body of a PR.
func (t *Translator) prInvolvesMayor(pr *gogithub.PullRequest) bool {
	if pr == nil {
		return false
	}
	if t.matchesLogin(pr.GetUser().GetLogin()) {
		return true
	}
	for _, u := range pr.Assignees {
		if t.matchesLogin(u.GetLogin()) {
			return true
		}
	}
	for _, u := range pr.RequestedReviewers {
		if t.matchesLogin(u.GetLogin()) {
			return true
		}
	}
	return t.bodyMentionsMayor(pr.GetBody())
}

// matchesLogin reports whether a GitHub login matches a configured mayor
// login (case-insensitive). Empty input is never a match.
func (t *Translator) matchesLogin(login string) bool {
	if login == "" {
		return false
	}
	_, ok := t.mayorLC[strings.ToLower(login)]
	return ok
}

// bodyMentionsMayor scans a comment/review/PR body for @<login> tokens
// matching mayor. GitHub mentions are case-insensitive, must be preceded by
// a non-identifier character (start-of-string, whitespace, punctuation),
// and run until the next non-identifier character. The regex below mirrors
// GitHub's actual parser closely enough for our purposes: a "@" preceded by
// a non-word boundary, then a login of 1–39 chars from [A-Za-z0-9-].
func (t *Translator) bodyMentionsMayor(body string) bool {
	if body == "" {
		return false
	}
	matches := ghMentionRE.FindAllStringSubmatchIndex(body, -1)
	for _, idx := range matches {
		if idx[2] < 0 {
			continue
		}
		login := body[idx[2]:idx[3]]
		if t.matchesLogin(login) {
			return true
		}
	}
	return false
}

// ghMentionRE captures a GitHub-style @login. The leading boundary uses a
// non-capturing group at the start of input or a non-word rune (Go's
// regexp lacks lookbehind).
var ghMentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))`)

// ---- per-event translators ----

func translatePullRequest(e *gogithub.PullRequestEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	pr := e.GetPullRequest()
	if pr == nil {
		return nil, fmt.Errorf("pull_request: missing pull_request field")
	}
	repo := e.GetRepo().GetFullName()
	subject := fmt.Sprintf("%s#%d", repo, pr.GetNumber())
	actor := e.GetSender().GetLogin()
	ts := timestampValue(pr.UpdatedAt)
	prText := titleAndBody(pr.GetTitle(), pr.GetBody())

	switch e.GetAction() {
	case "closed":
		// Telegraph cares about merges (the success path); a manual-close
		// without a merge is also routed through so Mayor can react.
		eventType := "pull_request.closed_unmerged"
		if pr.GetMerged() {
			eventType = "pull_request.merged"
			if mergedAt := timestampValue(pr.MergedAt); !mergedAt.IsZero() {
				ts = mergedAt
			}
		}
		return &telegraph.NormalizedEvent{
			Provider:     "github",
			EventType:    eventType,
			EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("pr-%d-%d", pr.GetNumber(), ts.UnixNano())),
			Actor:        actor,
			Subject:      subject,
			CanonicalURL: pr.GetHTMLURL(),
			Text:         prText,
			Labels:       prLabels(pr),
			Timestamp:    ts,
		}, nil
	case "opened", "reopened", "ready_for_review":
		return &telegraph.NormalizedEvent{
			Provider:     "github",
			EventType:    "pull_request.opened",
			EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("pr-%d-%d", pr.GetNumber(), ts.UnixNano())),
			Actor:        actor,
			Subject:      subject,
			CanonicalURL: pr.GetHTMLURL(),
			Text:         prText,
			Labels:       prLabels(pr),
			Timestamp:    ts,
		}, nil
	default:
		return nil, telegraph.ErrUnknownEventType
	}
}

func translatePullRequestReview(e *gogithub.PullRequestReviewEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	pr := e.GetPullRequest()
	if pr == nil {
		return nil, fmt.Errorf("pull_request_review: missing pull_request field")
	}
	review := e.GetReview()
	if review == nil {
		return nil, fmt.Errorf("pull_request_review: missing review field")
	}
	if e.GetAction() != "submitted" {
		// edited / dismissed are out of scope for v1 — they rarely add new
		// signal beyond the original submission.
		return nil, telegraph.ErrUnknownEventType
	}
	repo := e.GetRepo().GetFullName()
	subject := fmt.Sprintf("%s#%d", repo, pr.GetNumber())
	actor := review.GetUser().GetLogin()
	if actor == "" {
		actor = e.GetSender().GetLogin()
	}
	ts := timestampValue(review.SubmittedAt)
	state := strings.ToLower(strings.TrimSpace(review.GetState()))
	if state == "" {
		state = "commented"
	}
	text := review.GetBody()
	if text == "" {
		text = fmt.Sprintf("Review state: %s", state)
	}
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "pull_request.review_submitted",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("review-%d", review.GetID())),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: firstNonEmpty(review.GetHTMLURL(), pr.GetHTMLURL()),
		Text:         text,
		Labels:       []string{"review:" + state},
		Timestamp:    ts,
	}, nil
}

func translatePullRequestReviewComment(e *gogithub.PullRequestReviewCommentEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	pr := e.GetPullRequest()
	if pr == nil {
		return nil, fmt.Errorf("pull_request_review_comment: missing pull_request field")
	}
	comment := e.GetComment()
	if comment == nil {
		return nil, fmt.Errorf("pull_request_review_comment: missing comment field")
	}
	if e.GetAction() != "created" {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := e.GetRepo().GetFullName()
	subject := fmt.Sprintf("%s#%d", repo, pr.GetNumber())
	actor := comment.GetUser().GetLogin()
	ts := timestampValue(comment.CreatedAt)
	labels := []string{"review_comment"}
	if path := comment.GetPath(); path != "" {
		labels = append(labels, "path:"+path)
	}
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "pull_request.review_comment",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("review-comment-%d", comment.GetID())),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: firstNonEmpty(comment.GetHTMLURL(), pr.GetHTMLURL()),
		Text:         comment.GetBody(),
		Labels:       labels,
		Timestamp:    ts,
	}, nil
}

func translateIssueComment(e *gogithub.IssueCommentEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	issue := e.GetIssue()
	if issue == nil {
		return nil, fmt.Errorf("issue_comment: missing issue field")
	}
	comment := e.GetComment()
	if comment == nil {
		return nil, fmt.Errorf("issue_comment: missing comment field")
	}
	if e.GetAction() != "created" {
		return nil, telegraph.ErrUnknownEventType
	}
	// Restrict to PR comments — pure issue comments are out of Telegraph's
	// PR-centric remit. GitHub marks PR-issues with the pull_request link.
	if !issue.IsPullRequest() {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := e.GetRepo().GetFullName()
	subject := fmt.Sprintf("%s#%d", repo, issue.GetNumber())
	actor := comment.GetUser().GetLogin()
	ts := timestampValue(comment.CreatedAt)
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "pull_request.comment",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("issue-comment-%d", comment.GetID())),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: firstNonEmpty(comment.GetHTMLURL(), issue.GetHTMLURL()),
		Text:         comment.GetBody(),
		Labels:       []string{"comment"},
		Timestamp:    ts,
	}, nil
}

func translateCheckRun(e *gogithub.CheckRunEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	cr := e.GetCheckRun()
	if cr == nil {
		return nil, fmt.Errorf("check_run: missing check_run field")
	}
	if e.GetAction() != "completed" || !isFailureConclusion(cr.GetConclusion()) {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := e.GetRepo().GetFullName()
	subject := checkSubject(repo, cr.PullRequests, cr.GetHeadSHA())
	actor := e.GetSender().GetLogin()
	ts := timestampValue(cr.CompletedAt)
	text := fmt.Sprintf("Check run %q failed (conclusion=%s)", cr.GetName(), cr.GetConclusion())
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "check_run.failed",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("check-run-%d", cr.GetID())),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: cr.GetHTMLURL(),
		Text:         text,
		Labels:       []string{"conclusion:" + cr.GetConclusion(), "name:" + cr.GetName()},
		Timestamp:    ts,
	}, nil
}

func translateCheckSuite(e *gogithub.CheckSuiteEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	cs := e.GetCheckSuite()
	if cs == nil {
		return nil, fmt.Errorf("check_suite: missing check_suite field")
	}
	if e.GetAction() != "completed" || !isFailureConclusion(cs.GetConclusion()) {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := e.GetRepo().GetFullName()
	subject := checkSubject(repo, cs.PullRequests, cs.GetHeadSHA())
	actor := e.GetSender().GetLogin()
	ts := timestampValue(cs.UpdatedAt)
	canonical := ""
	if r := e.GetRepo(); r != nil && cs.GetHeadSHA() != "" {
		canonical = strings.TrimRight(r.GetHTMLURL(), "/") + "/commit/" + cs.GetHeadSHA()
	}
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "check_suite.failed",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("check-suite-%d", cs.GetID())),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: canonical,
		Text:         fmt.Sprintf("Check suite failed (conclusion=%s) for %s", cs.GetConclusion(), shortSHA(cs.GetHeadSHA())),
		Labels:       []string{"conclusion:" + cs.GetConclusion()},
		Timestamp:    ts,
	}, nil
}

func translateWorkflowRun(e *gogithub.WorkflowRunEvent, deliveryID string) (*telegraph.NormalizedEvent, error) {
	wr := e.GetWorkflowRun()
	if wr == nil {
		return nil, fmt.Errorf("workflow_run: missing workflow_run field")
	}
	if e.GetAction() != "completed" || !isFailureConclusion(wr.GetConclusion()) {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := e.GetRepo().GetFullName()
	subject := checkSubject(repo, wr.PullRequests, wr.GetHeadSHA())
	actor := e.GetSender().GetLogin()
	ts := timestampValue(wr.UpdatedAt)
	text := fmt.Sprintf("Workflow %q failed on %s (conclusion=%s)", wr.GetName(), wr.GetHeadBranch(), wr.GetConclusion())
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "workflow_run.failed",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("workflow-run-%d", wr.GetID())),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: wr.GetHTMLURL(),
		Text:         text,
		Labels:       []string{"conclusion:" + wr.GetConclusion(), "name:" + wr.GetName()},
		Timestamp:    ts,
	}, nil
}

// ---- helpers ----

// eventRepoFullName extracts repository.full_name from any of the typed
// event structs we accept. Returns "" when the event has no repository
// field or the repo's name is missing.
func eventRepoFullName(parsed interface{}) string {
	switch e := parsed.(type) {
	case *gogithub.PullRequestEvent:
		return e.GetRepo().GetFullName()
	case *gogithub.PullRequestReviewEvent:
		return e.GetRepo().GetFullName()
	case *gogithub.PullRequestReviewCommentEvent:
		return e.GetRepo().GetFullName()
	case *gogithub.IssueCommentEvent:
		return e.GetRepo().GetFullName()
	case *gogithub.CheckRunEvent:
		return e.GetRepo().GetFullName()
	case *gogithub.CheckSuiteEvent:
		return e.GetRepo().GetFullName()
	case *gogithub.WorkflowRunEvent:
		return e.GetRepo().GetFullName()
	}
	return ""
}

func prLabels(pr *gogithub.PullRequest) []string {
	if pr == nil || len(pr.Labels) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		if name := l.GetName(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func titleAndBody(title, body string) string {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	switch {
	case title != "" && body != "":
		return title + "\n\n" + body
	case title != "":
		return title
	default:
		return body
	}
}

// timestampValue returns the UTC time value of a (possibly nil) go-github
// Timestamp pointer. Unset/JSON-null inputs yield the zero time so callers
// can detect missing/malformed data explicitly — see the Jira translator
// for the same pattern. Substituting time.Now() on missing data would mask
// malformed payloads as "events that happened just now" and surface as
// drift in the Telegraph-Timestamp header on audit logs.
func timestampValue(ts *gogithub.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.UTC()
}

// deliveryOrFallback prefers the X-GitHub-Delivery UUID (stable, GitHub-issued)
// over a synthetic ID derived from payload fields. The synthetic form is only
// used in tests or when the header is absent.
func deliveryOrFallback(delivery, fallback string) string {
	if delivery != "" {
		return delivery
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// isFailureConclusion is the set of conclusions Telegraph treats as actionable
// for a "failing CI" notification. success / neutral / skipped are excluded
// (success is the happy path; neutral is informational; skipped means the
// check did not run). The remaining values are taken from the documented
// check_run / check_suite enumerations.
func isFailureConclusion(c string) bool {
	switch strings.ToLower(c) {
	case "failure", "timed_out", "cancelled", "action_required", "stale", "startup_failure":
		return true
	default:
		return false
	}
}

// checkSubject derives a Telegraph subject for a check-style event. When the
// check is associated with a PR, prefer "owner/repo#N"; otherwise fall back to
// "owner/repo@<sha7>" so the operator can correlate to a commit.
func checkSubject(repo string, prs []*gogithub.PullRequest, sha string) string {
	if len(prs) > 0 && prs[0].GetNumber() > 0 {
		return fmt.Sprintf("%s#%d", repo, prs[0].GetNumber())
	}
	if sha != "" {
		return fmt.Sprintf("%s@%s", repo, shortSHA(sha))
	}
	return repo
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
