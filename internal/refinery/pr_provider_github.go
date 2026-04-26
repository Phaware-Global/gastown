package refinery

import "github.com/steveyegge/gastown/internal/git"

// githubPRProvider implements PRProvider using the gh CLI via git.Git.
type githubPRProvider struct {
	git *git.Git
}

func newGitHubPRProvider(g *git.Git) PRProvider {
	return &githubPRProvider{git: g}
}

func (p *githubPRProvider) FindPRNumber(branch string) (int, error) {
	return p.git.FindPRNumber(branch)
}

func (p *githubPRProvider) IsPRApproved(prNumber int) (bool, error) {
	return p.git.IsPRApproved(prNumber)
}

func (p *githubPRProvider) IsPRApprovedBy(prNumber int, user string) (bool, error) {
	return p.git.GhPrApprovedBy(prNumber, user)
}

func (p *githubPRProvider) MergePR(prNumber int, method string) (string, error) {
	return p.git.GhPrMerge(prNumber, method)
}

func (p *githubPRProvider) CreatePR(opts CreatePROptions) (int, string, error) {
	return p.git.GhPrCreate(opts.Branch, opts.Base, opts.Title, opts.Body)
}

func (p *githubPRProvider) RequestReview(prNumber int, reviewers []string) error {
	return p.git.GhPrRequestReview(prNumber, reviewers)
}

func (p *githubPRProvider) UnresolvedThreads(prNumber int) ([]ReviewThread, error) {
	threads, err := p.git.GhPrUnresolvedThreads(prNumber)
	if err != nil {
		return nil, err
	}
	return gitReviewThreadsToProvider(threads), nil
}

func (p *githubPRProvider) AllThreads(prNumber int) ([]ReviewThread, error) {
	threads, err := p.git.GhPrReviewThreads(prNumber)
	if err != nil {
		return nil, err
	}
	return gitReviewThreadsToProvider(threads), nil
}

func (p *githubPRProvider) CountApprovals(prNumber int) (int, error) {
	return p.git.GhPrApprovalCount(prNumber)
}

func (p *githubPRProvider) ChecksRollup(prNumber int) (string, bool, error) {
	return p.git.GhPrChecksRollup(prNumber)
}

func (p *githubPRProvider) PostComment(prNumber int, body string) error {
	return p.git.GhPrComment(prNumber, body)
}

func (p *githubPRProvider) HasReviewFrom(prNumber int, user string) (bool, error) {
	return p.git.GhPrHasReviewFrom(prNumber, user)
}

func (p *githubPRProvider) ListReviewAuthors(prNumber int) ([]string, error) {
	return p.git.GhPrReviewAuthors(prNumber)
}

func (p *githubPRProvider) HasReviewFromOnSHA(prNumber int, user, sha string) (bool, error) {
	return p.git.GhPrHasReviewFromOnSHA(prNumber, user, sha)
}

func (p *githubPRProvider) CurrentHeadSHA(prNumber int) (string, error) {
	return p.git.GhPrHeadSHA(prNumber)
}
