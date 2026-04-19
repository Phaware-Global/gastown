package formula

import (
	"strings"
	"testing"
)

// TestRefineryPatrolMergePushHasPRBranch asserts the Phase 2 refactor of
// `mol-refinery-patrol`'s merge-push step: under merge_strategy=pr the
// formula must orchestrate PR operations via the `gt refinery pr …`
// subcommand tree, not by embedding raw `gh pr create` / `gh pr merge`
// plumbing. This guards against silent regression of the refactor.
//
// Covers the acceptance criterion for Phase 4 of the design
// (docs/design/refinery-pr-workflow.md): "end-to-end test passes without
// [the per-rig overlay]".
func TestRefineryPatrolMergePushHasPRBranch(t *testing.T) {
	data, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading formula: %v", err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing formula: %v", err)
	}

	var mergePush *Step
	for i := range f.Steps {
		if f.Steps[i].ID == "merge-push" {
			mergePush = &f.Steps[i]
			break
		}
	}
	if mergePush == nil {
		t.Fatal("merge-push step not found in mol-refinery-patrol")
	}
	desc := mergePush.Description

	// Phase 2 contract: the PR branch must drive the workflow through
	// `gt refinery pr …` subcommands. If any of these disappear, the
	// formula has silently regressed to embedding raw gh plumbing.
	mustContain := []string{
		`{{ cmd }} refinery pr create`,
		`{{ cmd }} refinery pr wait-ci`,
		`{{ cmd }} refinery pr request-review`,
		`{{ cmd }} refinery pr wait-approval`,
		`{{ cmd }} refinery pr merge`,
	}
	for _, pattern := range mustContain {
		if !strings.Contains(desc, pattern) {
			t.Errorf("merge-push description missing expected PR orchestration: %q\n(indicates Phase 2 regression)",
				pattern)
		}
	}

	// The direct-merge branch must still be present — strategy=direct
	// is the default and shipping without it would break every existing rig.
	directMarkers := []string{
		`If merge_strategy = "direct"`,
		`git merge --ff-only temp`,
		`git push origin <merge-target>`,
	}
	for _, marker := range directMarkers {
		if !strings.Contains(desc, marker) {
			t.Errorf("merge-push description missing direct-merge marker: %q\n(direct strategy is the default and must stay)",
				marker)
		}
	}

	// Negative assertion: the old raw-gh PR-create path must NOT appear
	// in the PR branch anymore. Allow the marker inside the direct path's
	// "check for open PR on branch" code (gas-fk4) — that's `gh pr list`,
	// not `gh pr create`. Matching `gh pr create --base` catches the
	// specific pattern the refactor removed.
	forbiddenInPR := []string{
		`gh pr create \\`, // the multi-line `gh pr create --base ... --head ... ` form
		`gh pr merge <polecat-branch>`,
		`gh pr checks <polecat-branch> --repo`,
	}
	for _, pattern := range forbiddenInPR {
		if strings.Contains(desc, pattern) {
			t.Errorf("merge-push description still contains pre-Phase-2 raw-gh plumbing: %q\n(should be replaced by `gt refinery pr …` subcommands)",
				pattern)
		}
	}
}

// TestRefineryPatrolHasMergeStrategyVar asserts the formula exposes the
// merge_strategy variable to operators. Without this, rigs can't opt into
// the PR workflow via rig settings + formula vars propagation.
func TestRefineryPatrolHasMergeStrategyVar(t *testing.T) {
	data, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading formula: %v", err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing formula: %v", err)
	}

	if _, ok := f.Vars["merge_strategy"]; !ok {
		t.Error("mol-refinery-patrol must declare a merge_strategy variable")
	}
	if _, ok := f.Vars["require_review"]; !ok {
		t.Error("mol-refinery-patrol must declare a require_review variable (kept for backward compatibility with Phase 1 rigs)")
	}
}
