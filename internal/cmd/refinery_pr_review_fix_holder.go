package cmd

import (
	"fmt"
	"os"
	"regexp"

	"github.com/steveyegge/gastown/internal/beads"
)

// reviewFixHolderStatuses are the bead states in which a bead still holds work.
// "hooked" is listed on its own because Beads.List only folds in hooked wisps
// when it is asked for that status by name.
var reviewFixHolderStatuses = []string{"open", "in_progress", "hooked", "blocked"}

// reviewFixHolderGate stops a review-fix dispatch for a PR another bead already
// holds. A second polecat on the same PR branch races the first one's pushes, and
// the generic "Review-fix: PR #N" bead this dispatch mints would be that second
// polecat's work. Nothing is dispatched or created; the next patrol looks again
// and the PR resumes once the holder finishes. Exit 1 is "patrol again"; a holder
// query that cannot be answered is operational (exit 4) rather than a go-ahead,
// because proceeding blind is exactly the duplicate this gate exists to prevent.
func reviewFixHolderGate(list func() ([]*beads.Issue, error), state dispatchReviewFixState, mrID string) error {
	issues, err := list()
	if err != nil {
		return wrapOperationalErr(fmt.Errorf("checking for a bead already holding PR #%d: %w", state.PRNumber, err))
	}
	holder := reviewFixHolder(issues, state, mrID)
	if holder == nil {
		return nil
	}
	who := holder.Assignee
	if who == "" {
		who = "unassigned"
	}
	fmt.Fprintf(os.Stdout,
		"PR #%d: review-fix already held by %s (%s, %s) — skipping dispatch; will look again next patrol\n",
		state.PRNumber, holder.ID, holder.Status, who)
	return NewSilentExit(1)
}

// listReviewFixHolderCandidates returns the live beads of the MR's rig database.
func listReviewFixHolderCandidates(mrID string) func() ([]*beads.Issue, error) {
	return func() ([]*beads.Issue, error) {
		bd := beads.New(resolveBeadDir(mrID))
		var all []*beads.Issue
		for _, status := range reviewFixHolderStatuses {
			issues, err := bd.List(beads.ListOptions{Status: status, Priority: -1})
			if err != nil {
				return nil, fmt.Errorf("listing %s beads: %w", status, err)
			}
			all = append(all, issues...)
		}
		return all, nil
	}
}

// reviewFixHolder returns a live bead, other than the MR and its source issue,
// that claims the PR's review-fix, or nil. A bead claims the PR when its title
// names it (scoped beads are free-form: "PR #345", "PR345"), its description has
// a review_pr field for it, or its description names the PR branch. A bare
// mention of the PR number in description prose is not a claim: any bead that
// merely discusses the PR would otherwise freeze its review loop.
//
// The source issue is excluded because re-slinging it is the normal dispatch path
// (reviewFixSourceHeldByWorkingPolecat guards a live holder there). An unassigned
// generic "Review-fix: PR #N" bead is an orphan from a sling that failed after the
// bead was created; it does not count, or one failed sling would stall the PR.
func reviewFixHolder(issues []*beads.Issue, state dispatchReviewFixState, mrID string) *beads.Issue {
	prTitle := regexp.MustCompile(fmt.Sprintf(`(?i)\bPR\s*#?%d\b`, state.PRNumber))
	prField := regexp.MustCompile(fmt.Sprintf(`(?m)^\s*review_pr:\s*%d\s*$`, state.PRNumber))
	var branchRef *regexp.Regexp
	if state.Branch != "" {
		branchRef = regexp.MustCompile(`(?:^|[^\w/.@-])` + regexp.QuoteMeta(state.Branch) + `(?:$|[^\w/.@-])`)
	}
	generic := fmt.Sprintf("Review-fix: PR #%d", state.PRNumber)

	for _, issue := range issues {
		if issue == nil || issue.ID == mrID || issue.ID == state.SourceIssue {
			continue
		}
		if issue.Status == "closed" || issue.Status == "tombstone" {
			continue
		}
		if hasLabel(issue.Labels, "gt:merge-request") {
			continue
		}
		if issue.Title == generic && issue.Assignee == "" {
			continue
		}
		if prTitle.MatchString(issue.Title) || prField.MatchString(issue.Description) ||
			(branchRef != nil && branchRef.MatchString(issue.Description)) {
			return issue
		}
	}
	return nil
}
