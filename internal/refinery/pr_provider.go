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
