package cmd

import (
	"testing"
)

// TestIsAuthorizedMainCommit covers the commit-subject classifier used by the
// main-integrity detector. The detector flags any commit on origin/main whose
// subject doesn't look like an authorised PR squash-merge — the primary
// indicator being the "(#N)" suffix that gh pr merge --squash appends.
//
// The incident that motivated this detector (gt-i71) used the commit message
// "fix: auto-save uncommitted implementation work (gt-pvx safety net)" — a
// message that has no PR number and is the exact pattern the auto-save hook
// produces. This test explicitly covers that shape as a negative case.
func TestIsAuthorizedMainCommit(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    bool // true = authorized, false = flagged as unauthorized
	}{
		// Authorised patterns — squash merge subjects with PR number.
		{"squash merge", "feat(polecat): add checkout-branch (#49)", true},
		{"squash merge with parens in title", "fix(gt-pvx): refuse auto-commit on protected branches (G41) (#48)", true},
		{"squash merge simple", "docs: update readme (#1)", true},
		{"squash merge large PR number", "chore: bump deps (#1023)", true},

		// Authorised patterns — non-squash but explicitly allowed shapes.
		{"revert commit", `Revert "fix: auto-save uncommitted implementation work (gt-pvx safety net)"`, true},
		{"merge upstream", "Merge upstream gastownhall/gastown into Phaware fork (18 commits)", true},
		{"merge branch", "Merge branch 'main' of github.com/Phaware-Global/gastown", true},
		{"initial commit", "Initial commit", true},
		{"initial lowercase", "initial commit", true},

		// Unauthorized patterns — the kinds that slip through without PR review.
		{
			"auto-save message (the gt-i71 incident shape)",
			"fix: auto-save uncommitted implementation work (gt-pvx safety net)",
			false,
		},
		{
			"auto-save with issue ref (variant)",
			"fix: auto-save uncommitted implementation work (gt-mwy.4, gt-pvx safety net)",
			false,
		},
		{"direct commit no PR", "fix: update config", false},
		{"has parens but not PR number", "feat: add thing (closes gt-abc)", false},
		{"paren with non-digit", "fix: stuff (#abc)", false},
		{"PR number not at end", "fix: add (#12) more stuff", false},
		{"empty", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAuthorizedMainCommit(tc.subject)
			if got != tc.want {
				t.Errorf("isAuthorizedMainCommit(%q) = %v, want %v", tc.subject, got, tc.want)
			}
		})
	}
}
