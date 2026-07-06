package refinery

import (
	"errors"

	"github.com/steveyegge/gastown/internal/git"
)

// ErrUnsupported is returned by a PRProvider that does not implement an
// optional method (e.g., Bitbucket doesn't yet support thread listing).
var ErrUnsupported = errors.New("operation not supported by this PR provider")

// CreatePROptions controls PR creation.
type CreatePROptions struct {
	Branch string // head branch
	Base   string // target branch (e.g. "main")
	Title  string
	Body   string
}

// ReviewThread is a provider-agnostic view of a single review thread on a PR.
type ReviewThread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       int
	Author     string
	Body       string
	URL        string
}

// PRProvider abstracts VCS-specific PR operations for the merge queue.
// Implementations exist for GitHub (default) and Bitbucket Cloud.
type PRProvider interface {
	// FindPRNumber returns the PR number/ID for the given branch, or 0 if none exists.
	FindPRNumber(branch string) (int, error)

	// IsPRApproved checks whether a PR has at least one approving review.
	IsPRApproved(prNumber int) (bool, error)

	// IsPRApprovedBy checks whether the given user has an APPROVED review that
	// has not been dismissed or superseded by a CHANGES_REQUESTED review.
	// Pass user="" to fall back to IsPRApproved.
	IsPRApprovedBy(prNumber int, user string) (bool, error)

	// MergePR merges a PR using the specified method (e.g., "squash", "merge", "rebase").
	// Returns the merge commit SHA on success (if available).
	MergePR(prNumber int, method string) (string, error)

	// CreatePR creates a PR, or returns the existing one if an open PR already
	// exists for opts.Branch. Returns the PR number and URL.
	CreatePR(opts CreatePROptions) (prNumber int, url string, err error)

	// RequestReview requests reviews from the given GitHub users / teams.
	// Repeated requests for the same reviewer are idempotent.
	RequestReview(prNumber int, reviewers []string) error

	// ChangesRequestedReviewers returns the logins of reviewers whose most
	// recent terminal review is CHANGES_REQUESTED — the reviewers currently
	// blocking the PR. A reviewer who later APPROVED (or whose changes-request
	// was DISMISSED) is excluded. Used by the await-review drift-reset path to
	// re-request blocking reviewers once per new HEAD, since GitHub does not
	// auto re-request a reviewer after a force-push. Providers that can't
	// enumerate review states return ErrUnsupported.
	ChangesRequestedReviewers(prNumber int) ([]string, error)

	// UnresolvedThreads returns all review threads that are not resolved
	// (outdated threads are included, since GitHub's
	// required_review_thread_resolution blocks merge on isResolved alone).
	UnresolvedThreads(prNumber int) ([]ReviewThread, error)

	// AllThreads returns every review thread on the PR, including resolved
	// and outdated ones. Used by callers that want the full picture rather
	// than the filtered unresolved list.
	AllThreads(prNumber int) ([]ReviewThread, error)

	// CountApprovals returns the number of distinct users whose most recent
	// terminal review on the PR is APPROVED. Reviews that have been dismissed
	// or superseded by a CHANGES_REQUESTED review from the same user do not
	// count. Used to enforce pr_required_approvals > 1.
	CountApprovals(prNumber int) (int, error)

	// ChecksRollup returns the CI status rollup for the PR:
	//   state: "SUCCESS", "FAILURE", "ERROR", "PENDING", "NO_CHECKS", or "" if unknown
	//   done:  true once every check has reached a terminal state
	// When no checks are configured on the PR, state="NO_CHECKS" and done=false
	// so callers decide whether the absence of checks is acceptable (rather
	// than the provider silently greenlighting the merge).
	ChecksRollup(prNumber int) (state string, done bool, err error)

	// PostComment posts body as a new top-level PR comment. Used by the
	// refinery's await-review step to post an optional external review-bot
	// trigger comment (e.g. "/gemini review") as an imperative gate before
	// polling. The comment is NOT deduplicated against prior comments —
	// callers that want idempotency must check existing comments first.
	PostComment(prNumber int, body string) error

	// HasReviewFrom returns true iff the given user has submitted any
	// review on the PR (any state — COMMENTED / APPROVED /
	// CHANGES_REQUESTED / DISMISSED). This is the "reviewer has engaged"
	// gate, distinct from IsPRApprovedBy (which is the approval gate).
	HasReviewFrom(prNumber int, user string) (bool, error)

	// ListReviewAuthors returns the unique GitHub logins of every user
	// who has submitted at least one review on the PR (any state). The
	// returned slice preserves the original case of each login and is
	// sorted in case-insensitive lexicographic order so operators reading
	// the patrol log don't see Copilot before augmentcode purely because
	// of the capital C. Used by the await-review timeout path to surface
	// "PR has reviews from: ..." in the patrol log so a misconfigured
	// pr_reviewer is self-evident rather than silent. Providers that
	// don't yet support this should return ErrUnsupported; callers
	// tolerate that by emitting the bare timeout message.
	ListReviewAuthors(prNumber int) ([]string, error)

	// HasReviewFromOnSHA is the SHA-scoped variant: returns true iff the
	// user reviewed the PR at the given commit SHA. After a force-push,
	// the unscoped HasReviewFrom would false-positive on a stale prior
	// review; this variant constrains the match to the commit currently
	// up for merge. Pass sha="" to fall back to HasReviewFrom semantics
	// (any commit).
	HasReviewFromOnSHA(prNumber int, user, sha string) (bool, error)

	// CurrentHeadSHA returns the current head commit OID of the PR's
	// source branch as known to the upstream provider — authoritative
	// over local refs or MR-bead state (which can drift after a
	// force-push). Used by callers that need an authoritative SHA to
	// pair with HasReviewFromOnSHA.
	CurrentHeadSHA(prNumber int) (string, error)

	// SubmitReview submits a single PR review with the disposition in in.Event
	// (APPROVE / REQUEST_CHANGES / COMMENT; empty defaults to COMMENT), an
	// optional top-level summary body and optional inline comment threads,
	// anchored to in.CommitID when set. This is the sole sanctioned
	// review-posting path for the in-town Reviewer role (P23-2376); raw
	// `gh pr review` is tap-guard-blocked. Providers that cannot post reviews
	// return ErrUnsupported.
	SubmitReview(prNumber int, in SubmitReviewInput) error
}

// ReviewComment is one inline review comment thread anchored to a file and
// line on the post-change (RIGHT) side of the diff.
type ReviewComment struct {
	Path string // repo-relative file path
	Line int    // 1-based line number in the changed file
	Body string // rendered comment body (priority badge + [perspective] tag + text)
}

// SubmitReviewInput is the payload for PRProvider.SubmitReview.
type SubmitReviewInput struct {
	// CommitID anchors the review to a specific head SHA so it is SHA-scoped
	// for the refinery's engagement gate. Empty submits against the PR's
	// current head as the provider sees it.
	CommitID string
	// Body is the top-level review summary (per-perspective verdicts + counts).
	Body string
	// Comments are the inline finding threads. May be empty (summary-only).
	Comments []ReviewComment
	// Event is the GitHub review disposition: "APPROVE", "REQUEST_CHANGES", or
	// "COMMENT". Empty defaults to "COMMENT". The in-town Reviewer sets this from
	// its findings so the GitHub verdict matches their severity (e.g. a
	// high-severity finding posts REQUEST_CHANGES rather than a silent COMMENT).
	// Providers that cannot post reviews ignore it.
	Event string
}

// gitReviewThreadsToProvider converts the git package representation to the
// provider-agnostic type.
func gitReviewThreadsToProvider(in []git.GhReviewThread) []ReviewThread {
	out := make([]ReviewThread, 0, len(in))
	for _, t := range in {
		out = append(out, ReviewThread{
			ID:         t.ID,
			IsResolved: t.IsResolved,
			IsOutdated: t.IsOutdated,
			Path:       t.Path,
			Line:       t.Line,
			Author:     t.Author,
			Body:       t.Body,
			URL:        t.URL,
		})
	}
	return out
}
