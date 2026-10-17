package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// mrGuardEnv supplies the git/GitHub facts the MR submit guards need. It is an
// interface of funcs so the decision logic is testable without gh or a repo.
type mrGuardEnv struct {
	// OpenPRs lists the repository's open PRs. An error means PR state is
	// unknown; the PR-dependent checks then fail open with a warning.
	OpenPRs func() ([]git.OpenPR, error)
	// RangePatchIDs returns the patch-id of each commit reachable from head
	// but not from base. Patch-ids, unlike SHAs, survive the rebase that
	// gt done performs before queuing.
	RangePatchIDs func(base, head string) ([]string, error)
}

// checkMRSubmitGuards refuses to queue an MR whose contents or target are
// inconsistent with the PRs already open on GitHub (gt-beuz). `gt done` derives
// an MR from origin/<target>..HEAD, which is the whole of an existing PR when
// the polecat worked on a copy of that PR's branch, and the merge queue is a
// path around every GitHub gate (draft status, stacked bases). It returns the
// refusal as an error and any fail-open notices as warnings.
func checkMRSubmitGuards(env mrGuardEnv, branch, target string) (warnings []string, err error) {
	// Target equal to source is self-referential regardless of GitHub state.
	if strings.TrimPrefix(target, "origin/") == branch {
		return nil, fmt.Errorf("refusing to queue MR: target branch %q equals the source branch", target)
	}

	prs, listErr := env.OpenPRs()
	if listErr != nil {
		return []string{fmt.Sprintf("could not list open PRs, skipping PR-based MR guards: %v", listErr)}, nil
	}

	// This branch's own PR, if any: its base is authoritative for the target,
	// and a draft PR must not be fast-pathed through the queue.
	// Fork PRs are skipped throughout: their head ref names a branch in the
	// fork, not in this repository, so they say nothing about this branch.
	for _, pr := range prs {
		if pr.IsCrossRepository || pr.HeadRefName != branch {
			continue
		}
		if pr.IsDraft {
			return nil, fmt.Errorf("refusing to queue MR: PR #%d for branch %s is a draft — a draft PR is not ready to merge", pr.Number, branch)
		}
		if pr.BaseRefName != "" && pr.BaseRefName != target {
			return nil, fmt.Errorf("refusing to queue MR: target %q does not match PR #%d's base %q (a stacked PR must not target another branch)", target, pr.Number, pr.BaseRefName)
		}
	}

	// Commits this MR would carry that already live on a different open PR.
	mine, rangeErr := env.RangePatchIDs("origin/"+target, "HEAD")
	if rangeErr != nil {
		return []string{fmt.Sprintf("could not compute patches in origin/%s..HEAD, skipping shared-commit guard: %v", target, rangeErr)}, nil
	}
	if len(mine) == 0 {
		return nil, nil
	}
	mineSet := make(map[string]struct{}, len(mine))
	for _, c := range mine {
		mineSet[c] = struct{}{}
	}
	var conflicts []string
	for _, pr := range prs {
		if pr.IsCrossRepository || pr.HeadRefName == branch {
			continue
		}
		theirs, err := env.RangePatchIDs("origin/"+target, "origin/"+pr.HeadRefName)
		if err != nil {
			continue // head not fetched locally; cannot prove overlap
		}
		shared := 0
		for _, c := range theirs {
			if _, ok := mineSet[c]; ok {
				shared++
			}
		}
		if shared == 0 {
			continue
		}
		state := ""
		if pr.IsDraft {
			state = ", DRAFT"
		}
		conflicts = append(conflicts, fmt.Sprintf("PR #%d (%s%s): %d shared patch(es)", pr.Number, pr.HeadRefName, state, shared))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, fmt.Errorf("refusing to queue MR: origin/%s..HEAD contains patches already on other open PRs — "+
			"this branch looks like a copy of an existing PR's branch; push your commit to that PR's branch instead of queuing it.\n  %s",
			target, strings.Join(conflicts, "\n  "))
	}
	return nil, nil
}

func realMRGuardEnv(g *git.Git) mrGuardEnv {
	return mrGuardEnv{OpenPRs: g.ListOpenPRs, RangePatchIDs: g.PatchIDsInRange}
}
