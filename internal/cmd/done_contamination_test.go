package cmd

import "testing"

func TestDoneContaminationBaseRef(t *testing.T) {
	tests := []struct {
		name           string
		defaultBranch  string
		explicitTarget string
		want           string
	}{
		{
			name:           "defaults to rig branch",
			defaultBranch:  "main",
			explicitTarget: "",
			want:           "origin/main",
		},
		{
			name:           "uses explicit target branch",
			defaultBranch:  "main",
			explicitTarget: "upstream-rebuild-main",
			want:           "origin/upstream-rebuild-main",
		},
		{
			name:           "avoids double origin prefix",
			defaultBranch:  "main",
			explicitTarget: "origin/upstream-rebuild-main",
			want:           "origin/upstream-rebuild-main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := doneContaminationBaseRef(tt.defaultBranch, tt.explicitTarget)
			if got != tt.want {
				t.Fatalf("doneContaminationBaseRef(%q, %q) = %q, want %q", tt.defaultBranch, tt.explicitTarget, got, tt.want)
			}
		})
	}
}

// TestResolveDoneBaseBranch_ExplicitTargetFlagWins covers gt-35un's
// highest-priority path: an explicit --target flag is the polecat's own
// declaration of its base branch, so it must be honored without a Dolt
// round-trip (and regardless of what issueID/cwd would otherwise resolve
// to).
func TestResolveDoneBaseBranch_ExplicitTargetFlagWins(t *testing.T) {
	prevTarget := doneTarget
	defer func() { doneTarget = prevTarget }()

	doneTarget = "origin/develop"
	if got := resolveDoneBaseBranch("/nonexistent", "gt-does-not-matter"); got != "develop" {
		t.Fatalf("resolveDoneBaseBranch with --target flag set = %q, want %q (origin/ prefix stripped)", got, "develop")
	}
}

// TestResolveDoneBaseBranch_NoFlagNoIssueFallsBackEmpty covers the safe
// default: with no --target flag and no issue to look up formula_vars on,
// resolveDoneBaseBranch must return "" so callers fall back to
// RemoteDefaultBranch() rather than guessing.
func TestResolveDoneBaseBranch_NoFlagNoIssueFallsBackEmpty(t *testing.T) {
	prevTarget := doneTarget
	defer func() { doneTarget = prevTarget }()

	doneTarget = ""
	if got := resolveDoneBaseBranch("/nonexistent", ""); got != "" {
		t.Fatalf("resolveDoneBaseBranch with no flag and no issueID = %q, want \"\"", got)
	}
}
