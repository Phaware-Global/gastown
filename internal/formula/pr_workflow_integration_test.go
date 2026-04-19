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

	// Gate assertion: the PR orchestration must live inside a branch that's
	// explicitly gated on merge_strategy="pr". Without this check, a
	// regression could move the `gt refinery pr …` calls outside the
	// conditional (e.g., accidentally running them on every merge path)
	// and the PR-subcommand checks below would still pass.
	prBranchMarker := `If merge_strategy = "pr":`
	prBranchIdx := strings.Index(desc, prBranchMarker)
	if prBranchIdx < 0 {
		t.Fatalf("merge-push description missing the %q gate marker — PR orchestration must be gated on merge_strategy", prBranchMarker)
	}
	prBranchDesc := desc[prBranchIdx:]

	// Phase 2 contract: the PR branch must drive the workflow through
	// `gt refinery pr …` subcommands. Scoped to prBranchDesc so we're
	// asserting orchestration under the strategy gate, not anywhere in
	// the step text.
	mustContainInPRBranch := []string{
		`{{ cmd }} refinery pr create`,
		`{{ cmd }} refinery pr wait-ci`,
		`{{ cmd }} refinery pr request-review`,
		`{{ cmd }} refinery pr wait-approval`,
		`{{ cmd }} refinery pr merge`,
	}
	for _, pattern := range mustContainInPRBranch {
		if !strings.Contains(prBranchDesc, pattern) {
			t.Errorf("PR-mode branch missing expected orchestration: %q\n(indicates Phase 2 regression — subcommand may have moved outside the merge_strategy=pr gate)",
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

	// Negative assertion: the old raw-gh PR-create/merge/checks calls must
	// not reappear inside the PR branch. TOML multi-line basic strings strip
	// `\`-newline line continuations on parse, so the original
	// `gh pr create --base ... --head ...` multi-line form lands as a single
	// logical line with no backslashes — check for the flag combo instead of
	// looking for a literal `\\` sequence (which never existed post-parse).
	forbiddenInPRBranch := []string{
		`gh pr create --base`,               // the multi-line gh pr create form
		`gh pr create --head`,               // either arg ordering
		`gh pr merge <polecat-branch>`,      // old named-arg merge call
		`gh pr checks <polecat-branch>`,     // old named-arg checks call
	}
	for _, pattern := range forbiddenInPRBranch {
		if strings.Contains(prBranchDesc, pattern) {
			t.Errorf("PR-mode branch still contains pre-Phase-2 raw-gh plumbing: %q\n(should be replaced by `gt refinery pr …` subcommands)",
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
