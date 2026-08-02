package reviewer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// These tests cover the disposition override's escalation-only contract. The
// override exists because normalizeFinding requires every finding to anchor to
// a path and a positive line, so an architectural objection — real, but with no
// single diff line to attach it to — cannot be expressed as a finding. Without
// an escape hatch such a pass posts as APPROVE with "do not merge" as its body.

func TestParseFindings_ApproveDispositionIsRejectedOutright(t *testing.T) {
	// Not just past a high finding: "approve" is absent from the closed set
	// entirely, so the Findings boundary enforces the same contract as
	// ParsePerspectiveResult and as every doc surface that describes it.
	for _, findings := range []string{
		`[{"path":"a.go","line":1,"priority":"high","title":"t","body":"b"}]`,
		`[{"path":"a.go","line":1,"priority":"medium","title":"t","body":"b"}]`,
		`[{"path":"a.go","line":1,"priority":"low","title":"t","body":"b"}]`,
		`[]`,
	} {
		payload := `{"summary":"s","disposition":"approve","findings":` + findings + `}`
		_, err := ParseFindings([]byte(payload))
		if err == nil {
			t.Errorf("approve disposition must be rejected regardless of severity (findings=%s)", findings)
			continue
		}
		if !strings.Contains(err.Error(), "approve") {
			t.Errorf("error should name the offending value, got: %v", err)
		}
	}
}

func TestParseFindings_ApproveRejectedEvenWithOddCasing(t *testing.T) {
	// The lookup lowercases and trims, so a casing variant must not slip past.
	payload := `{"summary":"s","disposition":"  ApPrOvE  ","findings":[]}`
	if _, err := ParseFindings([]byte(payload)); err == nil {
		t.Error("a case/space variant of approve must be rejected too")
	}
}

func TestReviewEvent_CommentDispositionCannotSuppressAHighFinding(t *testing.T) {
	// The regression that matters most. A "comment" disposition is a legitimate
	// ESCALATION for the lens that set it, but it must never pull a different
	// lens's high finding down from REQUEST_CHANGES. Under an override-wins rule
	// it did — no injection, no bad actor, just a perf lens following the
	// documented contract.
	fs := &Findings{
		Summary:     "s",
		Disposition: "comment",
		Findings:    []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}},
	}
	if ev := fs.ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("ReviewEvent = %q, want REQUEST_CHANGES — a comment disposition must not "+
			"average away a dissenting block", ev)
	}
}

func TestParseFindings_CommentWithHighIsAcceptedAndStillBlocks(t *testing.T) {
	// comment+high is a LEGITIMATE payload (one lens escalating its own clean
	// tally while another found something high), so it must parse — and the
	// floor must resolve it to the blocking event.
	payload := `{"summary":"s","disposition":"comment","findings":[
	  {"path":"a.go","line":1,"priority":"high","title":"t","body":"b"}]}`
	fs, err := ParseFindings([]byte(payload))
	if err != nil {
		t.Fatalf("comment+high must parse — rejecting it would break correct usage: %v", err)
	}
	if ev := fs.ReviewEvent(); ev != "REQUEST_CHANGES" {
		t.Errorf("ReviewEvent = %q, want REQUEST_CHANGES", ev)
	}
}

func TestReviewEvent_DispositionIsAFloorNeverACeiling(t *testing.T) {
	// Exhaustive over the closed set x the severity tally: the result must
	// always be the MORE blocking of the two, never less.
	high := []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}}
	med := []Finding{{Path: "a.go", Line: 1, Priority: "medium", Title: "t"}}
	low := []Finding{{Path: "a.go", Line: 1, Priority: "low", Title: "t"}}

	cases := []struct {
		disposition string
		findings    []Finding
		want        string
	}{
		{"", high, "REQUEST_CHANGES"},
		{"", med, "COMMENT"},
		{"", low, "APPROVE"},
		{"", nil, "APPROVE"},
		{"comment", low, "COMMENT"},          // escalates
		{"comment", nil, "COMMENT"},          // escalates
		{"comment", med, "COMMENT"},          // agrees
		{"comment", high, "REQUEST_CHANGES"}, // must NOT de-escalate
		{"request_changes", nil, "REQUEST_CHANGES"},
		{"request_changes", low, "REQUEST_CHANGES"},
		{"request_changes", med, "REQUEST_CHANGES"},
		{"request_changes", high, "REQUEST_CHANGES"},
	}
	for _, tc := range cases {
		fs := &Findings{Summary: "s", Disposition: tc.disposition, Findings: tc.findings}
		if got := fs.ReviewEvent(); got != tc.want {
			t.Errorf("disposition=%q findings=%v: ReviewEvent = %q, want %q",
				tc.disposition, prioritiesOf(tc.findings), got, tc.want)
		}
	}
}

func prioritiesOf(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Priority)
	}
	return out
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

func TestSchemaExamplesDoNotSeedAnActionableDisposition(t *testing.T) {
	// The perspective subagent is told to follow the output schema exactly and
	// not improvise. Every other value in that example is a self-describing
	// placeholder, so a literal `"disposition": "request_changes"` would be
	// copied verbatim by a clean pass — posting a blocking review with zero
	// findings, which nothing downstream can correct (the floor only escalates)
	// and which the Reviewer cannot clear itself (gh pr review and both GraphQL
	// review mutations are tap-guard-blocked).
	//
	// Guard both agent-facing schemas against regressing to a literal value.
	for _, path := range []string{
		"prompt/execution.md.tmpl",
		"../templates/roles/reviewer.md.tmpl",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		body := string(data)
		for _, bad := range []string{
			`"disposition": "request_changes"`,
			`"disposition": "comment"`,
			`"disposition":"request_changes"`,
			`"disposition":"comment"`,
		} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains a literal actionable disposition (%s) in its schema; "+
					"a pass copying the schema would post a false block — use a "+
					"self-describing placeholder like the one in cmd/reviewer.go's payload schema", path, bad)
			}
		}
		// The field must still be present, or the escalation path has no producer.
		if !strings.Contains(body, `"disposition"`) {
			t.Errorf("%s no longer documents `disposition` — the escalation path needs a producer", path)
		}
	}
}
