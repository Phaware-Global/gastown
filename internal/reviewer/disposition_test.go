package reviewer

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests cover the disposition override's escalation-only contract. The
// override exists because normalizeFinding requires every finding to anchor to
// a path and a positive line, so an architectural objection — real, but with no
// single diff line to attach it to — cannot be expressed as a finding. Without
// an escape hatch such a pass posts as APPROVE with "do not merge" as its body.

func TestParseFindings_ApproveDispositionWithHighFindingIsRejected(t *testing.T) {
	payload := `{
	  "summary": "s",
	  "disposition": "approve",
	  "findings": [
	    {"path": "a.go", "line": 1, "priority": "high", "title": "boom", "body": "b"}
	  ]
	}`
	_, err := ParseFindings([]byte(payload))
	if err == nil {
		t.Fatal("approve disposition with a high finding must be rejected — " +
			"de-escalation past a high finding is the shape a prompt injection takes")
	}
	if !strings.Contains(err.Error(), "disposition=approve") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestParseFindings_EscalatingDispositionsAreAccepted(t *testing.T) {
	for _, d := range []string{"request_changes", "comment"} {
		payload := `{"summary":"s","disposition":"` + d + `","findings":[
		  {"path":"a.go","line":1,"priority":"high","title":"t","body":"b"}]}`
		if _, err := ParseFindings([]byte(payload)); err != nil {
			t.Errorf("disposition %q with a high finding must be accepted (escalation is always safe): %v", d, err)
		}
	}
}

func TestParseFindings_ApproveDispositionWithoutHighIsAccepted(t *testing.T) {
	// An explicit approve on a low-only payload agrees with severity derivation,
	// so there is nothing to guard against.
	payload := `{"summary":"s","disposition":"approve","findings":[
	  {"path":"a.go","line":1,"priority":"low","title":"nit","body":"b"}]}`
	fs, err := ParseFindings([]byte(payload))
	if err != nil {
		t.Fatalf("approve with no high findings must be accepted: %v", err)
	}
	if ev := fs.ReviewEvent(); ev != "APPROVE" {
		t.Errorf("ReviewEvent = %q, want APPROVE", ev)
	}
}

func TestReviewEvent_ApproveCannotOverrideHighFinding(t *testing.T) {
	// Backstop for Findings built in code rather than through ParseFindings.
	fs := &Findings{
		Summary:     "s",
		Disposition: "approve",
		Findings:    []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}},
	}
	if ev := fs.ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("ReviewEvent = %q, want REQUEST_CHANGES — an in-code approve must not "+
			"override a high finding either", ev)
	}
}

func TestReviewEvent_EscalationOverridesCleanTally(t *testing.T) {
	// The whole point: zero findings would derive APPROVE, but the pass blocked.
	fs := &Findings{Summary: "BLOCK: architectural", Disposition: "request_changes"}
	if ev := fs.ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("ReviewEvent = %q, want REQUEST_CHANGES — an unanchorable block must be expressible", ev)
	}
}

func TestParsePerspectiveResult_RejectsApproveDisposition(t *testing.T) {
	payload := `{"perspective":"security","verdict":"looks fine","disposition":"approve","findings":[]}`
	_, err := ParsePerspectiveResult([]byte(payload))
	if err == nil {
		t.Fatal("a perspective may escalate its verdict, never de-escalate — approve must be rejected")
	}
	if !strings.Contains(err.Error(), "escalate") {
		t.Errorf("error should explain the escalation-only rule, got: %v", err)
	}
}

func TestParsePerspectiveResult_AcceptsEscalatingDisposition(t *testing.T) {
	payload := `{"perspective":"security","verdict":"BLOCK: unsafe credential handling","disposition":"request_changes","findings":[]}`
	r, err := ParsePerspectiveResult([]byte(payload))
	if err != nil {
		t.Fatalf("request_changes must be accepted: %v", err)
	}
	if r.Disposition != "request_changes" {
		t.Errorf("Disposition = %q, want request_changes", r.Disposition)
	}
}

func TestConsolidate_TakesMostBlockingDisposition(t *testing.T) {
	// One lens blocking must never be averaged away by quieter lenses.
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "no findings"},
		{Perspective: "security", Verdict: "BLOCK: architectural", Disposition: "request_changes"},
		{Perspective: "perf", Verdict: "minor", Disposition: "comment"},
	}
	fs := Consolidate(results, "sha")
	if fs.Disposition != "request_changes" {
		t.Errorf("Disposition = %q, want request_changes (most blocking wins)", fs.Disposition)
	}
	// And it must actually change the submitted event: with zero findings the
	// severity tally alone would have produced APPROVE.
	if ev := fs.ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("ReviewEvent = %q, want REQUEST_CHANGES", ev)
	}
}

func TestConsolidate_NoDispositionLeavesSeverityDerivationIntact(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "no findings"},
		{Perspective: "security", Verdict: "no findings"},
	}
	fs := Consolidate(results, "sha")
	if fs.Disposition != "" {
		t.Errorf("Disposition = %q, want empty when no perspective escalated", fs.Disposition)
	}
	if ev := fs.ReviewEvent(); ev != "APPROVE" {
		t.Errorf("ReviewEvent = %q, want APPROVE — a genuinely clean pass still approves", ev)
	}
}

func TestConsolidate_DispositionSurvivesTheRoundTripToPost(t *testing.T) {
	// The gap this closes: Consolidate is the ONLY producer of the payload the
	// role is told to post, and it previously never set Disposition, so the
	// documented override was unreachable through the sanctioned pipeline.
	// Assert the field survives marshal → ParseFindings, which is exactly the
	// `consolidate --out findings.json` → `post --findings findings.json` hop.
	fs := Consolidate([]PerspectiveResult{
		{Perspective: "security", Verdict: "BLOCK", Disposition: "request_changes"},
	}, "deadbeef")

	encoded, err := json.Marshal(fs)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseFindings(encoded)
	if err != nil {
		t.Fatalf("consolidate output must be valid post input: %v", err)
	}
	if reparsed.Disposition != "request_changes" {
		t.Errorf("Disposition lost across the consolidate→post hop: %q", reparsed.Disposition)
	}
	if ev := reparsed.ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("ReviewEvent after round-trip = %q, want REQUEST_CHANGES", ev)
	}
}
