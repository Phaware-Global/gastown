package cmd

import (
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/steveyegge/gastown/internal/beads"
)

// reviewFixHolderStatuses are the bead states in which a polecat is working a
// bead. open and blocked are not here: neither has a session behind it, so
// neither can race a second polecat's pushes. "hooked" is listed on its own
// because Beads.List only folds in hooked wisps when it is asked for that status
// by name.
var reviewFixHolderStatuses = []string{"in_progress", "hooked"}

// reviewFixHolderGate stops a review-fix dispatch for a PR another polecat is
// already working. A second polecat on the same PR branch races the first one's
// pushes, and the generic "Review-fix: PR #N" bead this dispatch mints would be
// that second polecat's work. Nothing is dispatched or created; the next patrol
// looks again and the PR resumes once the holder finishes.
//
// A bead that claims the PR holds it only while its assignee is a live polecat of
// this rig: a dead or absent assignee leaves nothing to race, and counting it
// would stall the PR for as long as the bead exists. A liveness lookup that
// fails is treated as held, like reviewFixSourceHeldByWorkingPolecat, because
// dispatching blind is the duplicate this gate exists to prevent. Every hold
// prints the holder bead and why, so a stalled PR is never silent.
//
// Exit 1 is "patrol again"; a holder query that cannot be answered is
// operational (exit 4) rather than a go-ahead.
func reviewFixHolderGate(list func() ([]*beads.Issue, error), alive func(polecat string) (bool, error), rigName string, state dispatchReviewFixState, mrID string) error {
	issues, err := list()
	if err != nil {
		return wrapOperationalErr(fmt.Errorf("checking for a bead already holding PR #%d: %w", state.PRNumber, err))
	}
	for _, holder := range reviewFixHolders(issues, state, mrID) {
		name, perr := polecatNameFromAssignee(rigName, holder.Assignee)
		if perr != nil {
			// Unassigned, or owned by a non-polecat or another rig: no polecat
			// of ours is working the PR through this bead.
			continue
		}
		live, aerr := alive(name)
		switch {
		case aerr != nil:
			fmt.Fprintf(os.Stdout,
				"PR #%d: review-fix held — %s (%s) is assigned to polecat %s and its liveness lookup failed (%v); treating it as held, will look again next patrol\n",
				state.PRNumber, holder.ID, holder.Status, name, aerr)
			return NewSilentExit(1)
		case live:
			fmt.Fprintf(os.Stdout,
				"PR #%d: review-fix held — %s (%s) is held by alive polecat %s; skipping dispatch, will look again next patrol\n",
				state.PRNumber, holder.ID, holder.Status, name)
			return NewSilentExit(1)
		}
	}
	return nil
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

// reviewFixHolders returns the in-flight beads, other than the MR and its source
// issue, that claim the PR's review-fix. A bead claims the PR when its title names
// it (scoped beads are free-form: "PR #345", "PR345"), its description has a
// review_pr field for it, or its description names the PR branch. A bare mention
// of the PR number in description prose is not a claim: any bead that merely
// discusses the PR would otherwise freeze its review loop. Whether a claim still
// has a live polecat behind it is for reviewFixHolderGate to decide.
//
// The source issue is excluded because re-slinging it is the normal dispatch path
// (reviewFixSourceHeldByWorkingPolecat guards a live holder there).
func reviewFixHolders(issues []*beads.Issue, state dispatchReviewFixState, mrID string) []*beads.Issue {
	prTitle := regexp.MustCompile(fmt.Sprintf(`(?i)\bPR\s*#?%d\b`, state.PRNumber))
	prField := regexp.MustCompile(fmt.Sprintf(`(?m)^\s*review_pr:\s*%d\s*$`, state.PRNumber))
	var branchRef *regexp.Regexp
	if state.Branch != "" {
		branchRef = regexp.MustCompile(`(?:^|[^\w/.@-])` + regexp.QuoteMeta(state.Branch) + `(?:$|[^\w/.@-])`)
	}

	var holders []*beads.Issue
	for _, issue := range issues {
		if issue == nil || issue.ID == mrID || issue.ID == state.SourceIssue {
			continue
		}
		if !slices.Contains(reviewFixHolderStatuses, issue.Status) {
			continue
		}
		if hasLabel(issue.Labels, "gt:merge-request") {
			continue
		}
		if prTitle.MatchString(issue.Title) || prField.MatchString(issue.Description) ||
			(branchRef != nil && branchRef.MatchString(issue.Description)) {
			holders = append(holders, issue)
		}
	}
	return holders
}
