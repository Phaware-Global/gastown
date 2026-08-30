package reviewer

import (
	"strings"
	"testing"
)

const sampleDiff = `diff --git a/libs/domain/survey-code.value.ts b/libs/domain/survey-code.value.ts
index 1111111..2222222 100644
--- a/libs/domain/survey-code.value.ts
+++ b/libs/domain/survey-code.value.ts
@@ -37,2 +37,4 @@ export const isSuppressedSurveyCode = (
+  const trimmed = code?.trim() ?? '';
+  if (!trimmed) return true;
@@ -80 +82,2 @@ export class SurveyCode {
+  readonly raw: string;
diff --git a/libs/infra/suppression.mongo-expr.ts b/libs/infra/suppression.mongo-expr.ts
index 3333333..4444444 100644
--- a/libs/infra/suppression.mongo-expr.ts
+++ b/libs/infra/suppression.mongo-expr.ts
@@ -12,3 +12,3 @@ export const expr = {
-  $trim: { input: field },
+  $trim: { input: field, chars: WS },
`

func TestParseDiffManifest(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)

	if len(m) != 2 {
		t.Fatalf("parsed %d files, want 2: %+v", len(m), m)
	}
	if !m.Touches("libs/domain/survey-code.value.ts") {
		t.Error("changed file not in manifest")
	}
	if m.Touches("libs/application/survey-notification.command-handler.ts") {
		t.Error("untouched file reported as changed")
	}

	// First hunk covers 37..40, second 82..83.
	for _, line := range []int{37, 40, 82, 83} {
		if !m.ChangedAt("libs/domain/survey-code.value.ts", line) {
			t.Errorf("line %d should be inside a changed hunk", line)
		}
	}
	for _, line := range []int{36, 41, 81, 84} {
		if m.ChangedAt("libs/domain/survey-code.value.ts", line) {
			t.Errorf("line %d is outside every hunk but reported as changed", line)
		}
	}
}

func TestParseDiffManifest_SkipsDeletedFiles(t *testing.T) {
	diff := `diff --git a/gone.ts b/gone.ts
--- a/gone.ts
+++ /dev/null
@@ -1,3 +0,0 @@
-a
`
	if m := ParseDiffManifest(diff); m.Touches("gone.ts") {
		t.Error("a deleted file has no post-image lines and must not be anchorable")
	}
}

func TestClassify_NilManifestChangesNothing(t *testing.T) {
	var m DiffManifest
	f := Finding{Path: "anything.ts", Line: 1}
	if got := m.Classify(f); got != ScopeUnknown {
		t.Errorf("nil manifest = %q, want ScopeUnknown — absent evidence must not demote", got)
	}
}

func TestClassify_AnchorPlacement(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	tests := []struct {
		name string
		f    Finding
		want Scope
	}{
		{"changed line", Finding{Path: "libs/domain/survey-code.value.ts", Line: 38}, ScopeIn},
		{"touched file, untouched line", Finding{Path: "libs/domain/survey-code.value.ts", Line: 200}, ScopeAdjacent},
		{"untouched file", Finding{Path: "libs/application/other.ts", Line: 5}, ScopeOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.Classify(tt.f); got != tt.want {
				t.Errorf("Classify = %q, want %q", got, tt.want)
			}
		})
	}
}

// The anchor-laundering case, taken from graphql-api PR #112 thread
// r3745217309: a HIGH anchored on a changed line whose remediation lived
// entirely in files the PR never touched.
func TestClassify_RemediationOutsideDiffOverridesGoodAnchor(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	f := Finding{
		Path:  "libs/domain/survey-code.value.ts",
		Line:  38, // genuinely a changed line
		Title: "Fail-closed flip silently gates a write",
		RemediationPaths: []string{
			"libs/application/survey-notification.command-handler.ts",
			"libs/domain/study-survey.value.ts",
		},
	}
	if got := m.Classify(f); got != ScopeOut {
		t.Errorf("Classify = %q, want out_of_scope: the anchor is in the diff but "+
			"every edit it demands is not", got)
	}
}

func TestClassify_RemediationInsideDiffKeepsScope(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	f := Finding{
		Path:             "libs/domain/survey-code.value.ts",
		Line:             38,
		RemediationPaths: []string{"libs/infra/suppression.mongo-expr.ts"},
	}
	if got := m.Classify(f); got != ScopeIn {
		t.Errorf("Classify = %q, want in_scope", got)
	}
}

func TestConsolidate_DemotesOutOfScopeToNonBlocking(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	results := []PerspectiveResult{{Perspective: "domain-driven-design", Verdict: "v", Findings: []Finding{{
		Path: "libs/domain/survey-code.value.ts", Line: 38, Priority: "high",
		Perspective:      "domain-driven-design",
		Title:            "Fail-closed flip silently gates a write",
		Body:             "Two untouched callers inherit the new behaviour.",
		RemediationPaths: []string{"libs/application/survey-notification.command-handler.ts"},
	}}}}

	fs := Consolidate(results, "sha", m)

	if len(fs.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs.Findings))
	}
	f := fs.Findings[0]
	if f.Scope != ScopeOut {
		t.Errorf("scope = %q, want out_of_scope", f.Scope)
	}
	if f.Priority != "low" {
		t.Errorf("priority = %q, want low (demoted)", f.Priority)
	}
	// Demoted, not dropped: the finding still posts and still says why.
	if !strings.Contains(f.Body, "Non-blocking") {
		t.Errorf("body should explain the demotion:\n%s", f.Body)
	}
	if !strings.Contains(f.Body, "Two untouched callers") {
		t.Errorf("original body was lost:\n%s", f.Body)
	}
	if !strings.Contains(f.Body, "survey-notification.command-handler.ts") {
		t.Errorf("notice should name the out-of-diff paths:\n%s", f.Body)
	}
	if ev := fs.ReviewEvent(); ev != "APPROVE" {
		t.Errorf("event = %q, want APPROVE — an out-of-scope finding must not block", ev)
	}
}

func TestConsolidate_InScopeHighStillBlocks(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	results := []PerspectiveResult{{Perspective: "security", Verdict: "v", Findings: []Finding{{
		Path: "libs/domain/survey-code.value.ts", Line: 38, Priority: "high",
		Perspective: "security", Title: "Real in-scope defect",
	}}}}
	if ev := Consolidate(results, "sha", m).ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("event = %q, want REQUEST_CHANGES", ev)
	}
}

// disposition=request_changes creates a merge block with no thread, which the
// thread-driven fix loop cannot clear without an operator. It is honoured only
// when the round also found something blocking inside the diff.
func TestConsolidate_UnanchoredBlockSoftenedWithoutInScopeHigh(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	results := []PerspectiveResult{{
		Perspective: "domain-driven-design",
		Verdict:     "BLOCK: architectural objection",
		Disposition: "request_changes",
		Findings: []Finding{{
			Path: "libs/application/untouched.ts", Line: 10, Priority: "high",
			Perspective: "domain-driven-design", Title: "Out of diff",
		}},
	}}

	fs := Consolidate(results, "sha", m)

	if fs.Disposition != "comment" {
		t.Errorf("disposition = %q, want comment", fs.Disposition)
	}
	if ev := fs.ReviewEvent(); ev != "COMMENT" {
		t.Errorf("event = %q, want COMMENT", ev)
	}
	// The objection still posts in full.
	if !strings.Contains(fs.Summary, "BLOCK: architectural objection") {
		t.Errorf("verdict text was lost:\n%s", fs.Summary)
	}
}

func TestConsolidate_UnanchoredBlockSurvivesWithInScopeHigh(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	results := []PerspectiveResult{{
		Perspective: "security", Verdict: "BLOCK", Disposition: "request_changes",
		Findings: []Finding{{
			Path: "libs/domain/survey-code.value.ts", Line: 38, Priority: "high",
			Perspective: "security", Title: "In-diff defect",
		}},
	}}
	if fs := Consolidate(results, "sha", m); fs.ReviewEvent() != "REQUEST_CHANGES" {
		t.Errorf("event = %q, want REQUEST_CHANGES", fs.ReviewEvent())
	}
}

// Without a manifest the softening must not fire: a reviewer running without
// diff data has no evidence the objection is out of scope.
func TestConsolidate_UnanchoredBlockKeptWhenScopeUnknown(t *testing.T) {
	results := []PerspectiveResult{{
		Perspective: "security", Verdict: "BLOCK", Disposition: "request_changes",
	}}
	if fs := Consolidate(results, "sha", nil); fs.ReviewEvent() != "REQUEST_CHANGES" {
		t.Errorf("event = %q, want REQUEST_CHANGES with no manifest", fs.ReviewEvent())
	}
}

// Regression: the (path, line) dedup merged Priority, Title, Body, Suggestion
// and Perspective but dropped the duplicate's RemediationPaths, so a lens that
// omitted them could mask a lens that named them honestly — and whether the
// anchor-laundering rail fired depended on subagent arrival order.
func TestConsolidate_MergeUnionsRemediationPaths(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	inDiff := "libs/domain/survey-code.value.ts"

	// Lens A arrives first with no remediation paths; lens B arrives second and
	// correctly names an out-of-diff path.
	results := []PerspectiveResult{
		{Perspective: "security", Verdict: "v", Findings: []Finding{{
			Path: inDiff, Line: 38, Priority: "medium", Perspective: "security",
			Title: "Vague concern",
		}}},
		{Perspective: "domain-driven-design", Verdict: "v", Findings: []Finding{{
			Path: inDiff, Line: 38, Priority: "high", Perspective: "domain-driven-design",
			Title:            "Fix lives in an untouched file",
			RemediationPaths: []string{"libs/application/untouched.ts"},
		}}},
	}

	fs := Consolidate(results, "sha", m)

	if len(fs.Findings) != 1 {
		t.Fatalf("got %d findings, want 1 merged", len(fs.Findings))
	}
	f := fs.Findings[0]
	if f.Scope != ScopeOut {
		t.Errorf("scope = %q, want out_of_scope: the second lens named an out-of-diff "+
			"remediation path and the merge must not discard it", f.Scope)
	}
	if ev := fs.ReviewEvent(); ev != "APPROVE" {
		t.Errorf("event = %q, want APPROVE — this must not block", ev)
	}
}

// The same asymmetry in the other arrival order must reach the same verdict.
func TestConsolidate_MergeUnionsRemediationPathsEitherOrder(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	inDiff := "libs/domain/survey-code.value.ts"

	results := []PerspectiveResult{
		{Perspective: "domain-driven-design", Verdict: "v", Findings: []Finding{{
			Path: inDiff, Line: 38, Priority: "high", Perspective: "domain-driven-design",
			Title:            "Fix lives in an untouched file",
			RemediationPaths: []string{"libs/application/untouched.ts"},
		}}},
		{Perspective: "security", Verdict: "v", Findings: []Finding{{
			Path: inDiff, Line: 38, Priority: "medium", Perspective: "security",
			Title: "Vague concern",
		}}},
	}

	if got := Consolidate(results, "sha", m).Findings[0].Scope; got != ScopeOut {
		t.Errorf("scope = %q, want out_of_scope regardless of arrival order", got)
	}
}

func TestMergeRemediationPaths(t *testing.T) {
	got := mergeRemediationPaths([]string{"a.ts", "b.ts"}, []string{" b.ts ", "", "c.ts"})
	want := []string{"a.ts", "b.ts", "c.ts"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v (first-seen order, deduped, blanks dropped)", got, want)
		}
	}
}

// OutOfScopeNotice must not label an in-diff path as outside the diff.
func TestOutOfScopeNotice_OnlyNamesPathsActuallyOutside(t *testing.T) {
	m := ParseDiffManifest(sampleDiff)
	inDiff := "libs/domain/survey-code.value.ts"

	t.Run("mixed paths name only the outside one", func(t *testing.T) {
		got := OutOfScopeNotice(m, Finding{
			Path: inDiff, Line: 38,
			RemediationPaths: []string{inDiff, "libs/application/untouched.ts"},
		})
		if strings.Contains(got, inDiff) {
			t.Errorf("in-diff path labelled as outside the diff:\n%s", got)
		}
		if !strings.Contains(got, "libs/application/untouched.ts") {
			t.Errorf("out-of-diff path not named:\n%s", got)
		}
	})

	t.Run("outside anchor with entirely in-diff remediation", func(t *testing.T) {
		got := OutOfScopeNotice(m, Finding{
			Path: "libs/application/untouched.ts", Line: 10,
			RemediationPaths: []string{inDiff},
		})
		if strings.Contains(got, inDiff) {
			t.Errorf("the only named path is in-diff; it must not be called outside:\n%s", got)
		}
		if !strings.Contains(got, "anchors outside the lines this PR changed") {
			t.Errorf("the anchor is the real reason and must be stated:\n%s", got)
		}
	})

	t.Run("both outside states both", func(t *testing.T) {
		got := OutOfScopeNotice(m, Finding{
			Path: "libs/application/untouched.ts", Line: 10,
			RemediationPaths: []string{"libs/application/other.ts"},
		})
		if !strings.Contains(got, "anchors outside") || !strings.Contains(got, "libs/application/other.ts") {
			t.Errorf("both reasons should appear:\n%s", got)
		}
	})
}
