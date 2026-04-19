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

	// ChecksRollup returns the CI status rollup for the PR:
	//   state: "SUCCESS", "FAILURE", "ERROR", "PENDING", or "" if unknown
	//   done:  true once every check has reached a terminal state
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
