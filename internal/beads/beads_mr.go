// Package beads provides merge request and gate utilities.
package beads

import (
	"strings"
)

// FindMRForBranch searches for an open merge-request bead for the given branch.
// Returns the MR bead if found, nil if not found.
// This enables idempotent `gt done` - if an MR already exists, we skip creation.
func (b *Beads) FindMRForBranch(branch string) (*Issue, error) {
	return b.findMRForBranch(branch, true)
}

// FindMRForBranchAny searches for a merge-request bead for the given branch
// across all statuses (open and closed). Used by recovery checks to determine
// if work was ever submitted to the merge queue. See #1035.
func (b *Beads) FindMRForBranchAny(branch string) (*Issue, error) {
	return b.findMRForBranch(branch, false)
}

// FindMRForBranchAndSHA searches for an open merge-request bead matching both
// the branch name AND the commit SHA. This is the correct dedup key: two MRs
// from the same branch but with different commit SHAs are distinct submissions
// (e.g., polecat fixed a gate failure and re-pushed). See GH#3032.
//
// Returns nil if no MR matches both branch and SHA. Callers should create a
// new MR in that case and supersede old MRs for the same source issue.
func (b *Beads) FindMRForBranchAndSHA(branch, commitSHA string) (*Issue, error) {
	// Reject empty branch input. Otherwise an MR bead whose description
	// parses successfully (has other MR fields) but lacks a branch: field
	// would have fields.Branch == "" and match branch == "", returning a
	// spurious "found" result. Callers always pass a real branch name;
	// fail fast if they don't.
	if branch == "" {
		return nil, nil
	}

	issues, err := b.ListMergeRequests(ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}

	for _, issue := range issues {
		if issue.Status == "closed" {
			continue
		}
		fields := ParseMRFields(issue)
		if fields == nil || fields.Branch != branch {
			continue
		}
		// Branch matches — check commit SHA.
		// If the MR has no commit_sha field (legacy), fall back to
		// branch-only match for backward compatibility.
		if fields.CommitSHA != "" && commitSHA != "" {
			if fields.CommitSHA != commitSHA {
				// Same branch but different SHA — this is a stale MR.
				// Don't return it; caller will create a new MR and supersede.
				continue
			}
		}
		return issue, nil
	}

	return nil, nil
}

// findMRForBranch searches the wisps table (Dolt) for a merge-request
// bead whose `branch:` field (parsed via ParseMRFields) matches the given
// branch name exactly.
//
// Uses status=all which includes all issue statuses with full descriptions.
// Ephemeral=true routes to the wisps table where MR beads live (GH#2446).
// When skipClosed is true, closed beads are excluded (for open-MR checks).
func (b *Beads) findMRForBranch(branch string, skipClosed bool) (*Issue, error) {
	issues, err := b.ListMergeRequests(ListOptions{
		Status: "all",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}
	return pickMRForBranch(issues, branch, skipClosed), nil
}

// pickMRForBranch is the pure matching logic exercised by tests: given a
// list of MR-labeled issues and a branch name, return the first open
// (or any, if skipClosed=false) MR whose parsed `branch:` field matches.
//
// Field extraction (via ParseMRFields) is deliberately chosen over the
// previous strings.HasPrefix(description, "branch: X\n") approach. The
// G11 dogfood on 2026-04-19 surfaced a real breakage of the old prefix
// match: an MR bead backfilled with a description whose first bytes were
// NOT exactly "branch: X\n" (common real shapes: a leading blank line, a
// different field order, a header comment, or fields interleaved with
// prose) failed the HasPrefix test and looked like "no MR for this
// branch". The caller then tried to create a duplicate MR, which in turn
// failed with "MR bead handoff failed" even though an MR for the exact
// branch existed. Parsing the structured field matches the intended
// semantics (one MR per branch) instead of an accidental encoding of it.
//
// Returns nil if no MR matches.
//
// Empty-branch input is rejected outright. Otherwise an MR bead whose
// description parses successfully (has other MR fields) but lacks a
// branch: field would have fields.Branch == "" and accidentally match
// branch == "". Callers always pass a real branch name; fail fast if
// they don't.
func pickMRForBranch(issues []*Issue, branch string, skipClosed bool) *Issue {
	if branch == "" {
		return nil
	}
	for _, issue := range issues {
		if skipClosed && issue.Status == "closed" {
			continue
		}
		fields := ParseMRFields(issue)
		if fields == nil {
			continue
		}
		if fields.Branch == branch {
			return issue
		}
	}
	return nil
}

// FindOpenMRsForIssue returns all open merge-request beads whose source_issue
// matches the given issue ID. Used to find prior attempts when re-dispatching
// an issue and to supersede old MRs when a new one is created.
func (b *Beads) FindOpenMRsForIssue(issueID string) ([]*Issue, error) {
	issues, err := b.ListMergeRequests(ListOptions{
		Status: "open",
		Label:  "gt:merge-request",
	})
	if err != nil {
		return nil, err
	}

	var matches []*Issue
	for _, issue := range issues {
		if MatchesMRSourceIssue(issue.Description, issueID) {
			matches = append(matches, issue)
		}
	}
	return matches, nil
}

// MatchesMRSourceIssue returns true if the MR description contains a
// source_issue field matching the given issue ID exactly. The trailing
// newline in the needle prevents partial ID matches (e.g., "gt-abc"
// must not match "gt-abcdef").
func MatchesMRSourceIssue(description, issueID string) bool {
	needle := "source_issue: " + issueID + "\n"
	return strings.Contains(description, needle)
}
