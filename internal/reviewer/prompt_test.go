package reviewer

import (
	"strings"
	"testing"
)

// buildBuiltin resolves a built-in perspective and builds its prompt, failing
// the test on any error. It exercises the real ResolvePerspective →
// BuildPerspectivePrompt path the `gt reviewer prompt` command uses.
func buildBuiltin(t *testing.T, p PromptParams, name string) string {
	t.Helper()
	rp, err := ResolvePerspective("", "", name)
	if err != nil {
		t.Fatalf("ResolvePerspective(%q): %v", name, err)
	}
	p.Perspective = rp.Name
	p.Lens = rp.Content
	out, err := BuildPerspectivePrompt(p)
	if err != nil {
		t.Fatalf("BuildPerspectivePrompt: %v", err)
	}
	return out
}

func TestBuildPerspectivePrompt_InjectsLensAndSharedContract(t *testing.T) {
	out := buildBuiltin(t, PromptParams{
		RigName:     "gastown",
		PR:          123,
		SHA:         "abc123def456",
		Round:       1,
		MaxFindings: 8,
	}, "adversarial")

	// Lens content (from the resolved perspective markdown).
	if !strings.Contains(out, "You are hostile to this change") {
		t.Error("prompt missing the adversarial lens content")
	}
	// Shared execution contract, injected via templating — these strings live
	// ONLY in execution.md.tmpl, not in the perspective markdown.
	for _, want := range []string{
		"Execution contract",
		"Review target — the exact diff",
		"codegraph_callers",
		"Evidence standard",
		"machine-readable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing shared-contract marker %q", want)
		}
	}
	// Deterministic parameters threaded through.
	if !strings.Contains(out, "abc123def456") {
		t.Error("prompt missing the reviewed SHA")
	}
	if !strings.Contains(out, "#123") {
		t.Error("prompt missing the PR number")
	}
	if !strings.Contains(out, "at most **8**") {
		t.Error("prompt missing the max-findings cap")
	}
	// The output JSON schema must name this perspective.
	if !strings.Contains(out, `"perspective": "adversarial"`) {
		t.Error("prompt missing the perspective-tagged output schema")
	}
}

func TestBuildPerspectivePrompt_RoundContext(t *testing.T) {
	round1 := buildBuiltin(t, PromptParams{RigName: "gastown", PR: 1, SHA: "deadbeef", Round: 1}, "security")
	if !strings.Contains(round1, "round 1") {
		t.Error("round-1 prompt should state it is round 1")
	}
	if strings.Contains(round1, "Prior threads:") {
		t.Error("round-1 prompt should not include a prior-threads block")
	}

	round2 := buildBuiltin(t, PromptParams{
		RigName:      "gastown",
		PR:           1,
		SHA:          "deadbeef",
		Round:        2,
		PriorThreads: "- internal/foo.go:10 [bot] unchecked error",
	}, "security")
	if !strings.Contains(round2, "round 2") {
		t.Error("round-2 prompt should state it is round 2")
	}
	if !strings.Contains(round2, "Prior threads:") {
		t.Error("round-2 prompt should include the prior-threads block")
	}
	if !strings.Contains(round2, "unchecked error") {
		t.Error("round-2 prompt should embed the supplied prior threads")
	}
}

func TestBuildPerspectivePrompt_ExtraInstructionsSlot(t *testing.T) {
	out := buildBuiltin(t, PromptParams{
		RigName:           "gastown",
		PR:                7,
		SHA:               "cafef00d",
		Round:             1,
		ExtraInstructions: "Pay special attention to the new retry loop.",
	}, "adversarial")
	if !strings.Contains(out, "Additional instructions") {
		t.Error("prompt missing the extra-instructions header")
	}
	if !strings.Contains(out, "new retry loop") {
		t.Error("prompt missing the injected extra instructions")
	}

	// With no extra instructions, the section is omitted entirely.
	none := buildBuiltin(t, PromptParams{RigName: "gastown", PR: 7, SHA: "cafef00d", Round: 1}, "adversarial")
	if strings.Contains(none, "Additional instructions") {
		t.Error("extra-instructions section should be omitted when empty")
	}
}

func TestBuildPerspectivePrompt_FileBackedPerspective(t *testing.T) {
	rig := t.TempDir()
	writePerspective(t, rig, "go-idioms", "# Go idioms lens\n\nFlag non-idiomatic Go.")
	rp, err := ResolvePerspective("", rig, "go-idioms")
	if err != nil {
		t.Fatalf("ResolvePerspective: %v", err)
	}
	if rp.Source != PerspectiveSourceRig {
		t.Fatalf("expected rig source, got %s", rp.Source)
	}
	out, err := BuildPerspectivePrompt(PromptParams{
		Perspective: rp.Name, Lens: rp.Content, RigName: "gastown", PR: 2, SHA: "abc", Round: 1,
	})
	if err != nil {
		t.Fatalf("BuildPerspectivePrompt: %v", err)
	}
	if !strings.Contains(out, "Flag non-idiomatic Go") {
		t.Error("prompt missing file-backed lens content")
	}
	if !strings.Contains(out, `"perspective": "go-idioms"`) {
		t.Error("prompt schema not tagged with the file-backed perspective name")
	}
}

func TestBuildPerspectivePrompt_DefaultsMaxFindings(t *testing.T) {
	out := buildBuiltin(t, PromptParams{RigName: "gastown", PR: 1, SHA: "abc", Round: 1, MaxFindings: 0}, "adversarial")
	// 0 (unset) falls back to config.DefaultMaxFindingsPerPerspective (8).
	if !strings.Contains(out, "at most **8**") {
		t.Error("unset max-findings should default to 8")
	}
}

func TestBuildPerspectivePrompt_Validation(t *testing.T) {
	if _, err := BuildPerspectivePrompt(PromptParams{Perspective: "", Lens: "x", SHA: "a", PR: 1}); err == nil {
		t.Error("expected error for empty perspective name")
	}
	if _, err := BuildPerspectivePrompt(PromptParams{Perspective: "adversarial", Lens: "  ", SHA: "a", PR: 1}); err == nil {
		t.Error("expected error for empty lens content")
	}
	if _, err := BuildPerspectivePrompt(PromptParams{Perspective: "adversarial", Lens: "x", SHA: "a", PR: 0}); err == nil {
		t.Error("expected error for non-positive PR number")
	}
	if _, err := BuildPerspectivePrompt(PromptParams{Perspective: "adversarial", Lens: "x", SHA: "", PR: 1}); err == nil {
		t.Error("expected error for empty SHA")
	}
}
