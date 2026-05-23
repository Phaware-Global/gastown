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
package github

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
// ignore list. Both are silent drops at L3 with audit logging at the dispatcher.
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

	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("github: parsing webhook payload: %w", err)
	}

	var (
		evt *telegraph.NormalizedEvent
		err error
	)
	switch wireEvent {
	case "pull_request":
		evt, err = translatePullRequest(&p, deliveryID)
	case "pull_request_review":
		evt, err = translatePullRequestReview(&p, deliveryID)
	case "pull_request_review_comment":
		evt, err = translatePullRequestReviewComment(&p, deliveryID)
	case "issue_comment":
		evt, err = translateIssueComment(&p, deliveryID)
	case "check_run":
		evt, err = translateCheckRun(&p, deliveryID)
	case "check_suite":
		evt, err = translateCheckSuite(&p, deliveryID)
	case "workflow_run":
		evt, err = translateWorkflowRun(&p, deliveryID)
	default:
		t.logger.Info("telegraph: unknown github wire event",
			"wire_event", wireEvent, "delivery_id", deliveryID)
		return nil, telegraph.ErrUnknownEventType
	}

	if errors.Is(err, telegraph.ErrUnknownEventType) {
		t.logger.Info("telegraph: github action not in scope",
			"wire_event", wireEvent, "action", p.Action, "delivery_id", deliveryID)
		return nil, telegraph.ErrUnknownEventType
	}
	if err != nil {
		return nil, err
	}

	// Repo filter: GitHub repository.full_name is the canonical "owner/repo".
	if t.allowedRepos != nil {
		repoName := strings.ToLower(repoFullName(&p))
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
	if len(t.ignoreActors) > 0 {
		if _, filtered := t.ignoreActors[evt.Actor]; filtered {
			return evt, telegraph.ErrActorFiltered
		}
	}

	// Relevance filter: drop events that do not pertain to mayor. Only applied
	// when a mayor identity is configured; an empty identity disables this
	// stage so existing operator deployments without mayor.* set behave as
	// before. wireEvent is needed to interpret involvement for check-style
	// events that lack PR objects.
	if t.mayor.HasAny() && !t.isRelevant(wireEvent, &p) {
		return evt, telegraph.ErrNotRelevant
	}

	return evt, nil
}

// isRelevant reports whether the parsed GitHub payload pertains to mayor.
//
// Involvement is established from any of:
//   - PR author (pull_request.user.login) == mayor
//   - mayor in pull_request.assignees
//   - mayor in pull_request.requested_reviewers
//   - mayor is the sender / actor of the event (sender.login or comment author)
//   - PR body @-mentions mayor
//   - the triggering comment / review body @-mentions mayor
//
// For check_run, check_suite, and workflow_run, GitHub does not carry the PR
// author or reviewer set on the payload — only a pull_requests stub with the
// PR number. Without state-tracking we cannot verify mayor PR involvement
// from the payload alone, so we apply the documented conservative rule:
//
//   - CI events with no PR association at all (push-to-branch, scheduled
//     workflows) are dropped. There is no PR-author signal to filter on, so
//     forwarding them creates org-wide CI noise.
//   - CI events that *do* have a PR association are allowed through, on the
//     trust boundary established by the operator's repo allow-list. This is
//     a best-effort filter — mayor may still see CI noise on collaborators'
//     PRs in the same repo. A stricter rule would require state, deferred.
//
// Relevance filtering is enforced in production: config validation
// requires a non-empty mayor.github_logins when the provider is enabled.
// The "empty identity disables filtering" branch in this translator is a
// test-only seam — production deployments never hit it because Validate()
// rejects an empty identity before the daemon starts.
func (t *Translator) isRelevant(wireEvent string, p *payload) bool {
	switch wireEvent {
	case "check_run":
		if p.CheckRun == nil || len(p.CheckRun.PullRequests) == 0 {
			return false
		}
		return true
	case "check_suite":
		if p.CheckSuite == nil || len(p.CheckSuite.PullRequests) == 0 {
			return false
		}
		return true
	case "workflow_run":
		if p.WorkflowRun == nil || len(p.WorkflowRun.PullRequests) == 0 {
			return false
		}
		return true
	}

	if t.matchesLogin(senderLogin(p)) {
		return true
	}
	if p.Comment != nil && t.matchesLogin(userLogin(p.Comment.User)) {
		return true
	}
	if p.Review != nil && t.matchesLogin(userLogin(p.Review.User)) {
		return true
	}

	if pr := p.PullRequest; pr != nil {
		if t.matchesLogin(userLogin(pr.User)) {
			return true
		}
		for _, u := range pr.Assignees {
			if t.matchesLogin(u.Login) {
				return true
			}
		}
		for _, u := range pr.RequestedReviewers {
			if t.matchesLogin(u.Login) {
				return true
			}
		}
		if t.bodyMentionsMayor(pr.Body) {
			return true
		}
	}

	// issue_comment on a PR carries the PR author in issue.user and PR
	// assignees in issue.assignees (GitHub's webhook never inlines the
	// full pull_request object on issue_comment deliveries). Without
	// these checks, comments on a Mayor-authored or Mayor-assigned PR
	// by other users — and without an @-mention — would be dropped as
	// not_relevant.
	if p.Issue != nil && p.Issue.PullRequest != nil {
		if t.matchesLogin(userLogin(p.Issue.User)) {
			return true
		}
		for _, u := range p.Issue.Assignees {
			if t.matchesLogin(u.Login) {
				return true
			}
		}
	}

	if p.Comment != nil && t.bodyMentionsMayor(p.Comment.Body) {
		return true
	}
	if p.Review != nil && t.bodyMentionsMayor(p.Review.Body) {
		return true
	}
	return false
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
		// idx[2:4] is the submatch positions of the login (group 1).
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
// negative lookahead alternative implemented as a non-capturing group at the
// start of input or a non-word rune (Go's regexp lacks lookbehind).
var ghMentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))`)

// ---- internal payload types ----

type payload struct {
	Action      string         `json:"action"`
	Sender      *ghUser        `json:"sender"`
	Repository  *ghRepo        `json:"repository"`
	PullRequest *ghPR          `json:"pull_request"`
	Issue       *ghIssue       `json:"issue"`
	Comment     *ghComment     `json:"comment"`
	Review      *ghReview      `json:"review"`
	CheckRun    *ghCheckRun    `json:"check_run"`
	CheckSuite  *ghCheckSuite  `json:"check_suite"`
	WorkflowRun *ghWorkflowRun `json:"workflow_run"`
}

type ghUser struct {
	Login string `json:"login"`
}

type ghRepo struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

// Time fields are kept as strings — GitHub sends them as RFC3339 timestamps
// or JSON null depending on the entity's state (e.g. merged_at is null on an
// open PR). Decoding null into time.Time is fragile across Go versions; parsing
// strings on demand is the same pattern the Jira translator uses.

type ghPR struct {
	Number             int       `json:"number"`
	HTMLURL            string    `json:"html_url"`
	Title              string    `json:"title"`
	Body               string    `json:"body"`
	State              string    `json:"state"`
	Merged             bool      `json:"merged"`
	MergedAt           string    `json:"merged_at"`
	UpdatedAt          string    `json:"updated_at"`
	User               *ghUser   `json:"user"`
	Labels             []ghLabel `json:"labels"`
	Head               *ghRef    `json:"head"`
	Assignees          []ghUser  `json:"assignees"`
	RequestedReviewers []ghUser  `json:"requested_reviewers"`
}

type ghIssue struct {
	Number      int       `json:"number"`
	HTMLURL     string    `json:"html_url"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	UpdatedAt   string    `json:"updated_at"`
	Labels      []ghLabel `json:"labels"`
	PullRequest *ghPRRef  `json:"pull_request"` // present iff this issue *is* a PR
	User        *ghUser   `json:"user"`         // PR author when this issue is a PR
	Assignees   []ghUser  `json:"assignees"`    // PR assignees when this issue is a PR
}

type ghPRRef struct {
	HTMLURL string `json:"html_url"`
}

type ghComment struct {
	ID        int64   `json:"id"`
	HTMLURL   string  `json:"html_url"`
	Body      string  `json:"body"`
	Path      string  `json:"path,omitempty"`
	User      *ghUser `json:"user"`
	UpdatedAt string  `json:"updated_at"`
	CreatedAt string  `json:"created_at"`
}

type ghReview struct {
	ID          int64   `json:"id"`
	HTMLURL     string  `json:"html_url"`
	State       string  `json:"state"` // approved | changes_requested | commented | dismissed
	Body        string  `json:"body"`
	User        *ghUser `json:"user"`
	SubmittedAt string  `json:"submitted_at"`
}

type ghCheckRun struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	HTMLURL      string           `json:"html_url"`
	Status       string           `json:"status"`     // queued | in_progress | completed
	Conclusion   string           `json:"conclusion"` // success | failure | timed_out | ...
	HeadSHA      string           `json:"head_sha"`
	CompletedAt  string           `json:"completed_at"`
	PullRequests []ghPRStub       `json:"pull_requests"`
	CheckSuite   *ghCheckSuiteRef `json:"check_suite"`
}

type ghCheckSuite struct {
	ID           int64      `json:"id"`
	HeadSHA      string     `json:"head_sha"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	UpdatedAt    string     `json:"updated_at"`
	PullRequests []ghPRStub `json:"pull_requests"`
}

type ghCheckSuiteRef struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ghWorkflowRun struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	HTMLURL      string     `json:"html_url"`
	HeadSHA      string     `json:"head_sha"`
	HeadBranch   string     `json:"head_branch"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	Event        string     `json:"event"`
	UpdatedAt    string     `json:"updated_at"`
	PullRequests []ghPRStub `json:"pull_requests"`
}

type ghPRStub struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// ---- per-event translators ----

func translatePullRequest(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.PullRequest == nil {
		return nil, fmt.Errorf("pull_request: missing pull_request field")
	}
	repo := repoFullName(p)
	subject := fmt.Sprintf("%s#%d", repo, p.PullRequest.Number)
	actor := senderLogin(p)
	ts := parseGHTime(p.PullRequest.UpdatedAt)
	prText := titleAndBody(p.PullRequest.Title, p.PullRequest.Body)

	switch p.Action {
	case "closed":
		// Telegraph cares about merges (the success path); a manual-close
		// without a merge is also routed through so Mayor can react.
		eventType := "pull_request.closed_unmerged"
		if p.PullRequest.Merged {
			eventType = "pull_request.merged"
			if mergedAt := parseGHTime(p.PullRequest.MergedAt); !mergedAt.IsZero() {
				ts = mergedAt
			}
		}
		return &telegraph.NormalizedEvent{
			Provider:     "github",
			EventType:    eventType,
			EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("pr-%d-%d", p.PullRequest.Number, ts.UnixNano())),
			Actor:        actor,
			Subject:      subject,
			CanonicalURL: p.PullRequest.HTMLURL,
			Text:         prText,
			Labels:       prLabels(p.PullRequest),
			Timestamp:    ts,
		}, nil
	case "opened", "reopened", "ready_for_review":
		return &telegraph.NormalizedEvent{
			Provider:     "github",
			EventType:    "pull_request.opened",
			EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("pr-%d-%d", p.PullRequest.Number, ts.UnixNano())),
			Actor:        actor,
			Subject:      subject,
			CanonicalURL: p.PullRequest.HTMLURL,
			Text:         prText,
			Labels:       prLabels(p.PullRequest),
			Timestamp:    ts,
		}, nil
	default:
		return nil, telegraph.ErrUnknownEventType
	}
}

func translatePullRequestReview(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.PullRequest == nil {
		return nil, fmt.Errorf("pull_request_review: missing pull_request field")
	}
	if p.Review == nil {
		return nil, fmt.Errorf("pull_request_review: missing review field")
	}
	if p.Action != "submitted" {
		// edited / dismissed are out of scope for v1 — they rarely add new
		// signal beyond the original submission.
		return nil, telegraph.ErrUnknownEventType
	}
	repo := repoFullName(p)
	subject := fmt.Sprintf("%s#%d", repo, p.PullRequest.Number)
	actor := userLogin(p.Review.User)
	if actor == "" {
		actor = senderLogin(p)
	}
	ts := parseGHTime(p.Review.SubmittedAt)
	state := strings.ToLower(strings.TrimSpace(p.Review.State))
	if state == "" {
		state = "commented"
	}
	text := p.Review.Body
	if text == "" {
		text = fmt.Sprintf("Review state: %s", state)
	}
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "pull_request.review_submitted",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("review-%d", p.Review.ID)),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: firstNonEmpty(p.Review.HTMLURL, p.PullRequest.HTMLURL),
		Text:         text,
		Labels:       []string{"review:" + state},
		Timestamp:    ts,
	}, nil
}

func translatePullRequestReviewComment(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.PullRequest == nil {
		return nil, fmt.Errorf("pull_request_review_comment: missing pull_request field")
	}
	if p.Comment == nil {
		return nil, fmt.Errorf("pull_request_review_comment: missing comment field")
	}
	if p.Action != "created" {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := repoFullName(p)
	subject := fmt.Sprintf("%s#%d", repo, p.PullRequest.Number)
	actor := userLogin(p.Comment.User)
	ts := parseGHTime(p.Comment.CreatedAt)
	labels := []string{"review_comment"}
	if p.Comment.Path != "" {
		labels = append(labels, "path:"+p.Comment.Path)
	}
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "pull_request.review_comment",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("review-comment-%d", p.Comment.ID)),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: firstNonEmpty(p.Comment.HTMLURL, p.PullRequest.HTMLURL),
		Text:         p.Comment.Body,
		Labels:       labels,
		Timestamp:    ts,
	}, nil
}

func translateIssueComment(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.Issue == nil {
		return nil, fmt.Errorf("issue_comment: missing issue field")
	}
	if p.Comment == nil {
		return nil, fmt.Errorf("issue_comment: missing comment field")
	}
	if p.Action != "created" {
		return nil, telegraph.ErrUnknownEventType
	}
	// Restrict to PR comments — pure issue comments are out of Telegraph's
	// PR-centric remit. GitHub flags PR-issues with issue.pull_request set.
	if p.Issue.PullRequest == nil {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := repoFullName(p)
	subject := fmt.Sprintf("%s#%d", repo, p.Issue.Number)
	actor := userLogin(p.Comment.User)
	ts := parseGHTime(p.Comment.CreatedAt)
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "pull_request.comment",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("issue-comment-%d", p.Comment.ID)),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: firstNonEmpty(p.Comment.HTMLURL, p.Issue.HTMLURL),
		Text:         p.Comment.Body,
		Labels:       []string{"comment"},
		Timestamp:    ts,
	}, nil
}

func translateCheckRun(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.CheckRun == nil {
		return nil, fmt.Errorf("check_run: missing check_run field")
	}
	if p.Action != "completed" || !isFailureConclusion(p.CheckRun.Conclusion) {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := repoFullName(p)
	subject := checkSubject(repo, p.CheckRun.PullRequests, p.CheckRun.HeadSHA)
	actor := senderLogin(p)
	ts := parseGHTime(p.CheckRun.CompletedAt)
	text := fmt.Sprintf("Check run %q failed (conclusion=%s)", p.CheckRun.Name, p.CheckRun.Conclusion)
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "check_run.failed",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("check-run-%d", p.CheckRun.ID)),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: p.CheckRun.HTMLURL,
		Text:         text,
		Labels:       []string{"conclusion:" + p.CheckRun.Conclusion, "name:" + p.CheckRun.Name},
		Timestamp:    ts,
	}, nil
}

func translateCheckSuite(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.CheckSuite == nil {
		return nil, fmt.Errorf("check_suite: missing check_suite field")
	}
	if p.Action != "completed" || !isFailureConclusion(p.CheckSuite.Conclusion) {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := repoFullName(p)
	subject := checkSubject(repo, p.CheckSuite.PullRequests, p.CheckSuite.HeadSHA)
	actor := senderLogin(p)
	ts := parseGHTime(p.CheckSuite.UpdatedAt)
	canonical := ""
	if p.Repository != nil && p.CheckSuite.HeadSHA != "" {
		canonical = strings.TrimRight(p.Repository.HTMLURL, "/") + "/commit/" + p.CheckSuite.HeadSHA
	}
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "check_suite.failed",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("check-suite-%d", p.CheckSuite.ID)),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: canonical,
		Text:         fmt.Sprintf("Check suite failed (conclusion=%s) for %s", p.CheckSuite.Conclusion, shortSHA(p.CheckSuite.HeadSHA)),
		Labels:       []string{"conclusion:" + p.CheckSuite.Conclusion},
		Timestamp:    ts,
	}, nil
}

func translateWorkflowRun(p *payload, deliveryID string) (*telegraph.NormalizedEvent, error) {
	if p.WorkflowRun == nil {
		return nil, fmt.Errorf("workflow_run: missing workflow_run field")
	}
	if p.Action != "completed" || !isFailureConclusion(p.WorkflowRun.Conclusion) {
		return nil, telegraph.ErrUnknownEventType
	}
	repo := repoFullName(p)
	subject := checkSubject(repo, p.WorkflowRun.PullRequests, p.WorkflowRun.HeadSHA)
	actor := senderLogin(p)
	ts := parseGHTime(p.WorkflowRun.UpdatedAt)
	text := fmt.Sprintf("Workflow %q failed on %s (conclusion=%s)", p.WorkflowRun.Name, p.WorkflowRun.HeadBranch, p.WorkflowRun.Conclusion)
	return &telegraph.NormalizedEvent{
		Provider:     "github",
		EventType:    "workflow_run.failed",
		EventID:      deliveryOrFallback(deliveryID, fmt.Sprintf("workflow-run-%d", p.WorkflowRun.ID)),
		Actor:        actor,
		Subject:      subject,
		CanonicalURL: p.WorkflowRun.HTMLURL,
		Text:         text,
		Labels:       []string{"conclusion:" + p.WorkflowRun.Conclusion, "name:" + p.WorkflowRun.Name},
		Timestamp:    ts,
	}, nil
}

// ---- helpers ----

func repoFullName(p *payload) string {
	if p == nil || p.Repository == nil {
		return ""
	}
	return p.Repository.FullName
}

func senderLogin(p *payload) string {
	if p == nil {
		return ""
	}
	return userLogin(p.Sender)
}

func userLogin(u *ghUser) string {
	if u == nil {
		return ""
	}
	return u.Login
}

func prLabels(pr *ghPR) []string {
	if pr == nil || len(pr.Labels) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		if l.Name != "" {
			out = append(out, l.Name)
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

// parseGHTime parses a GitHub-issued RFC3339 timestamp string. Empty, JSON
// "null", or unparseable inputs return the zero time so callers can detect
// missing/malformed data explicitly (see Jira's parseJiraTime for the same
// pattern). Returning time.Now() here would mask malformed payloads as
// "events that happened just now," which would surface as drift in the
// Telegraph-Timestamp header on tests and audit logs.
func parseGHTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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
func checkSubject(repo string, prs []ghPRStub, sha string) string {
	if len(prs) > 0 && prs[0].Number > 0 {
		return fmt.Sprintf("%s#%d", repo, prs[0].Number)
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
