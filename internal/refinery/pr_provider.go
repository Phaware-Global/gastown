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

	// UnresolvedThreads returns all review threads on the PR that are not
	// resolved and not outdated.
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
	// refinery's await-review step to post the reviewer-bot trigger
	// comment (e.g. "augment review") as an imperative gate before
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
