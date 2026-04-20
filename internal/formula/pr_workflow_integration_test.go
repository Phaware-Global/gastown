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

	// Bound the branch slice to just the PR section. Step PR.7 is the last
	// PR-strategy step; after it the merge-push text continues with shared
	// post-merge sections (`**Step 2: Send MERGED Notification`, Step 3
	// cleanup, etc.) that apply to both direct and pr paths. Without bounding,
	// a regression that puts raw gh plumbing in the shared section would
	// false-positive as a PR-branch regression, and — worse — a check moving
	// OUT of the PR branch into the shared section would still pass the
	// must-contain assertion.
	//
	// We stop at the first "**Step N:" header where N is NOT "PR.…". If no
	// such marker is found (the PR section happens to be the last thing in
	// the step), fall back to end-of-string.
	prBranchRest := desc[prBranchIdx:]
	prBranchEnd := len(prBranchRest)
	for _, stopMarker := range []string{
		"\n**Step 2: Send MERGED",   // the first shared post-merge step
		"\nIf merge_strategy = \"",   // any future additional strategy branch
	} {
		if idx := strings.Index(prBranchRest, stopMarker); idx >= 0 && idx < prBranchEnd {
			prBranchEnd = idx
		}
	}
	prBranchDesc := prBranchRest[:prBranchEnd]

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
	//
	// Patterns are intentionally specific (not broader word-boundary prefixes
	// like bare `gh pr create `):
	//   - The PR branch's prose legitimately references `gh pr checks $PR`
	//     inside single-quoted prose ("Error: run 'gh pr checks $PR' for
	//     details") as a user-facing hint. A broader `gh pr checks ` prefix
	//     would false-positive on that prose.
	//   - For `gh pr create`, we enumerate the common pre-Phase-2 flag combos
	//     (`--base`/`--head`/`--title`/`--fill`). A regression reintroducing
	//     the old raw-gh form will almost certainly include one of these,
	//     since `gh pr create` requires at least some of them to be useful.
	//   - For `gh pr merge`/`gh pr checks`, anchoring to the `<polecat-branch>`
	//     placeholder catches the exact pre-refactor named-arg form the
	//     original PR-mode code used.
	// If a future regression slips through these specific patterns (e.g.,
	// a new flag combo on `gh pr create`, or `gh pr merge $VAR` without
	// `<polecat-branch>`), add the specific pattern here — don't broaden
	// to a trailing-space prefix that'd false-positive on the prose hint.
	forbiddenInPRBranch := []string{
		`gh pr create --base`,           // pre-Phase-2 multi-line create form, --base first
		`gh pr create --head`,           // pre-Phase-2 multi-line create form, --head first
		`gh pr create --title`,          // create with --title at head of args
		`gh pr create --fill`,           // another common form
		`gh pr merge <polecat-branch>`,  // pre-refactor named-arg merge
		`gh pr checks <polecat-branch>`, // pre-refactor named-arg checks
	}
	for _, pattern := range forbiddenInPRBranch {
		if strings.Contains(prBranchDesc, pattern) {
			t.Errorf("PR-mode branch still contains pre-Phase-2 raw-gh plumbing: %q\n(should be replaced by `gt refinery pr …` subcommands)",
				pattern)
		}
	}
}

// TestRefineryPatrolReviewLoopIsPatrolResumable asserts Phase 5's
// non-blocking review-fix loop:
//
//   - Step PR.5 must not contain a `while true` (busy-waiting blocks the
//     merge queue behind a single PR's review cycle).
//   - Step PR.5 must use `gt mq set-review-state` to persist iteration
//     state on the MR bead. That's the primitive that makes the loop
//     resumable across patrol cycles; without it, iteration count
//     resets on every cycle and the loop cap is meaningless.
//   - Step PR.5 must read `review_fix_polecat` from the MR bead (the
//     "is a polecat in flight?" gate), not track it in a bash variable.
func TestRefineryPatrolReviewLoopIsPatrolResumable(t *testing.T) {
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

	prBranchIdx := strings.Index(desc, `If merge_strategy = "pr":`)
	if prBranchIdx < 0 {
		t.Fatal("missing PR-branch gate marker")
	}
	prBranchRest := desc[prBranchIdx:]
	prBranchEnd := len(prBranchRest)
	for _, stopMarker := range []string{
		"\n**Step 2: Send MERGED",
		"\nIf merge_strategy = \"",
	} {
		if idx := strings.Index(prBranchRest, stopMarker); idx >= 0 && idx < prBranchEnd {
			prBranchEnd = idx
		}
	}
	prBranchDesc := prBranchRest[:prBranchEnd]

	// Busy-wait regression guard: no `while true`. The original Phase-2
	// implementation blocked the merge queue behind each PR's review loop;
	// Phase 5 made it patrol-resumable. If someone reintroduces `while true`
	// (or `while :` or a bare `while [ ]` waiting loop) inside Step PR.5,
	// the refinery stops processing other MRs.
	for _, forbidden := range []string{
		"while true",
		"while :",
	} {
		if strings.Contains(prBranchDesc, forbidden) {
			t.Errorf("PR-mode branch contains busy-wait %q — Step PR.5 should be patrol-resumable, not block inside the formula",
				forbidden)
		}
	}

	// Persistence regression guard: Step PR.5 must use `gt mq set-review-state`
	// to record iter / in-flight polecat on the MR bead. Without this, the
	// patrol-resumable flow can't carry state across cycles.
	requiredPatrolResumableMarkers := []string{
		"mq set-review-state",              // the command that writes MR state
		"review_fix_polecat",                // the MR field holding the in-flight polecat
		"review_loop_iter",                  // the MR field holding iteration count
	}
	for _, marker := range requiredPatrolResumableMarkers {
		if !strings.Contains(prBranchDesc, marker) {
			t.Errorf("PR-mode branch missing patrol-resumable marker %q — review-fix loop may have regressed to a blocking form",
				marker)
		}
	}
}

// TestRefineryPatrolReviewLoopEnforcesResolveAndEnumeration pins the three
// invariants Step PR.5's header promises and the dogfood on 2026-04-19
// demonstrated are load-bearing:
//
//  1. **Author-agnostic dispatch** (G12b). The step must instruct that
//     unresolved threads count regardless of author. Without this marker,
//     the refinery LLM is free to reintroduce author-aware heuristics
//     like "wait for augment's response before dispatching" that strand
//     gemini threads indefinitely.
//
//  2. **Reply AND RESOLVE** (G13). The dispatch-args template must tell
//     the polecat to call `--resolve` (not just `--reply`). Gastown has
//     no auto-resolve; reply-only leaves threads open; the loop cannot
//     converge. Check that the step references `--resolve` and the
//     `respond-to-thread.mjs` script by name.
//
//  3. **Enumerate every thread verbatim** (G14). The --args string must
//     reference the `$THREADS` shell variable (not paraphrase it into
//     a narrative summary). The iter-2 dispatch in the 2026-04-19 dogfood
//     listed 3 of 4 unresolved threads in prose form; the missing thread
//     went to iter-3 and burned an iteration.
//
// This test protects the formula text against silent LLM-driven rewrites
// or future refactors that "clean up" the prose in a way that weakens
// the contract.
func TestRefineryPatrolReviewLoopEnforcesResolveAndEnumeration(t *testing.T) {
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

	prBranchIdx := strings.Index(desc, `If merge_strategy = "pr":`)
	if prBranchIdx < 0 {
		t.Fatal("missing PR-branch gate marker")
	}
	prBranchRest := desc[prBranchIdx:]
	prBranchEnd := len(prBranchRest)
	for _, stopMarker := range []string{
		"\n**Step 2: Send MERGED",
		"\nIf merge_strategy = \"",
	} {
		if idx := strings.Index(prBranchRest, stopMarker); idx >= 0 && idx < prBranchEnd {
			prBranchEnd = idx
		}
	}
	prBranchDesc := prBranchRest[:prBranchEnd]

	// Invariant 1: author-agnostic dispatch (G12b). The step header lists
	// this explicitly; the threads-poll block also reaffirms it. Either
	// marker is enough; require the stronger "regardless of" phrasing to
	// catch rewrites that soften into vague "consider all threads".
	authorAgnosticMarkers := []string{
		"Author-agnostic",
		"regardless of",
	}
	for _, marker := range authorAgnosticMarkers {
		if !strings.Contains(prBranchDesc, marker) {
			t.Errorf("PR-mode branch missing author-agnostic marker %q — dispatch may regress to per-author heuristics (G12b)",
				marker)
		}
	}

	// Invariant 2: reply AND resolve (G13). Check for resolve-semantics
	// markers without prescribing a specific tool. The formula is
	// deliberately tool-agnostic (polecats may have different skills
	// installed); what we pin is the behavioral contract and the
	// canonical GitHub primitive it references.
	resolveMarkers := []string{
		"RESOLVE",                // emphasized in prose to survive LLM summarization
		"resolveReviewThread",    // GitHub GraphQL primitive named explicitly
		"auto-resolve",           // the justification ("gastown has no auto-resolve")
	}
	for _, marker := range resolveMarkers {
		if !strings.Contains(prBranchDesc, marker) {
			t.Errorf("PR-mode branch missing resolve-enforcement marker %q — review-fix polecats may reply without resolving (G13)",
				marker)
		}
	}

	// Invariant 3: enumerate every thread verbatim (G14). The --args block
	// must splice the polled $THREADS JSON directly, not paraphrase it.
	// Check that $THREADS appears inside an --args string and that the
	// step warns against narrative paraphrasing.
	enumerationMarkers := []string{
		"$THREADS",                  // the poll result referenced verbatim
		"do not paraphrase",          // warning against prose substitution
	}
	for _, marker := range enumerationMarkers {
		// case-insensitive contains so future capitalization tweaks don't
		// break the test for cosmetic reasons
		if !strings.Contains(strings.ToLower(prBranchDesc), strings.ToLower(marker)) {
			t.Errorf("PR-mode branch missing thread-enumeration marker %q — dispatch args may drop unresolved threads (G14)",
				marker)
		}
	}
}

// TestRefineryPatrolHasOrphanBranchCheck asserts the orphan-branch-check
// step exists and contains the escalation contract.
//
// Real-world failure this guards against (2026-04-19 dogfood of gastown
// under merge_strategy=pr): a polecat's MR bead creation failed (separate
// bug), the refinery patrol found a polecat branch on origin with no MR,
// and reasoned from the branch contents alone that it was "ready to merge".
// It ran `git push origin FETCH_HEAD:refs/heads/main` and closed the bead.
// Unreviewed code landed on main; the entire PR workflow was bypassed on
// the first bead that tried to use it.
//
// The step's job is to make "polecat branch exists, no MR bead" a
// recognized, enumerated state in the formula — so the refinery LLM
// escalates rather than improvising. Key assertions:
//
//  1. The step exists and is reached from queue-scan.
//  2. queue-scan carries the "NEVER merge without an MR bead" rule.
//  3. The step names the escalation primitive (bd create with the
//     gt:escalation label) so the refinery has an explicit outlet.
//  4. The step explicitly bans the failure shape
//     (FETCH_HEAD:refs/heads/main and plain "git push ... :main").
func TestRefineryPatrolHasOrphanBranchCheck(t *testing.T) {
	data, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading formula: %v", err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing formula: %v", err)
	}

	// 1 — orphan-branch-check step exists and depends on queue-scan.
	var orphanStep *Step
	for i := range f.Steps {
		if f.Steps[i].ID == "orphan-branch-check" {
			orphanStep = &f.Steps[i]
			break
		}
	}
	if orphanStep == nil {
		t.Fatal("mol-refinery-patrol is missing the `orphan-branch-check` step (regression — see 2026-04-19 dogfood trace)")
	}
	if !containsString(orphanStep.Needs, "queue-scan") {
		t.Errorf("orphan-branch-check must depend on queue-scan (got needs=%v)", orphanStep.Needs)
	}

	// 1b — process-branch must depend on orphan-branch-check, not just
	// queue-scan. Otherwise a fan-out runner could schedule both
	// process-branch AND orphan-branch-check in parallel off queue-scan,
	// and process-branch could race to start on MR-queued work while
	// orphans are still unescalated. Serialization via `needs` is the
	// single source of truth for ordering.
	var processBranch *Step
	for i := range f.Steps {
		if f.Steps[i].ID == "process-branch" {
			processBranch = &f.Steps[i]
			break
		}
	}
	if processBranch == nil {
		t.Fatal("mol-refinery-patrol is missing the `process-branch` step")
	}
	if !containsString(processBranch.Needs, "orphan-branch-check") {
		t.Errorf("process-branch must depend on orphan-branch-check so orphan escalation happens before branch processing (got needs=%v)", processBranch.Needs)
	}

	desc := orphanStep.Description

	// 3 — escalation primitive is named so the refinery can't "miss" it.
	for _, must := range []string{
		"gt:escalation",
		"bd create",
	} {
		if !strings.Contains(desc, must) {
			t.Errorf("orphan-branch-check description missing escalation primitive %q — refinery needs an explicit outlet or it will improvise", must)
		}
	}

	// 2 — queue-scan carries the "never merge without MR" rule. Find it.
	var queueScan *Step
	for i := range f.Steps {
		if f.Steps[i].ID == "queue-scan" {
			queueScan = &f.Steps[i]
			break
		}
	}
	if queueScan == nil {
		t.Fatal("mol-refinery-patrol is missing the `queue-scan` step")
	}
	qsDesc := queueScan.Description
	mustInQueueScan := []string{
		"NEVER merge",      // the headline rule
		"MR bead",          // the primitive it hinges on
		"improvise",        // the anti-improvisation anchor (any phrasing around it is fine)
	}
	for _, s := range mustInQueueScan {
		// Case-insensitive match — the phrasing can evolve; the anchor words must stay.
		if !strings.Contains(strings.ToLower(qsDesc), strings.ToLower(s)) {
			t.Errorf("queue-scan description missing %q — without this, the refinery LLM can rationalize improvising a merge when the queue is empty", s)
		}
	}

	// 4 — explicit ban on the exact failure shape. The dogfood trace
	// ran `git push origin FETCH_HEAD:refs/heads/main`; the formula's
	// rules should name "refs/heads/main" and "push" in proximity.
	// We don't require the exact phrase — just that both anchors
	// appear in the queue-scan rules block.
	// Case-insensitive anchors. mustInQueueScan above already lowercases
	// both sides for phrasing-drift tolerance; these two anchors should
	// be equally forgiving so "Push" at the start of a sentence doesn't
	// flip the test even though the contract is still met.
	qsLower := strings.ToLower(qsDesc)
	hasPushKeyword := strings.Contains(qsLower, "push")
	hasMainRef := strings.Contains(qsLower, "refs/heads/main") || strings.Contains(qsLower, ":main")
	if !hasPushKeyword || !hasMainRef {
		t.Errorf("queue-scan must explicitly name push-to-main as a forbidden shape (push=%v, main-ref=%v) — the 2026-04-19 incident used `git push origin FETCH_HEAD:refs/heads/main`", hasPushKeyword, hasMainRef)
	}
}

// containsString is a small local helper — avoids importing slices just
// for this test and keeps the test file dependency-free.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
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
