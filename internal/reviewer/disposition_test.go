package reviewer

import (
	"encoding/json"
	"os"
	"regexp"
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
		payload := `{"summary":` + okSummary + `,"disposition":"approve","findings":` + findings + `}`
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
	payload := `{"summary":` + okSummary + `,"disposition":"  ApPrOvE  ","findings":[]}`
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
		Summary:     summaryOf("s"),
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
	payload := `{"summary":` + okSummary + `,"disposition":"comment","findings":[
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
		{"", med, "APPROVE"}, // severity alone never yields COMMENT
		{"", low, "APPROVE"},
		{"", nil, "APPROVE"},
		{"comment", low, "COMMENT"},          // escalates
		{"comment", nil, "COMMENT"},          // escalates
		{"comment", med, "COMMENT"},          // escalates: COMMENT is now reachable only this way
		{"comment", high, "REQUEST_CHANGES"}, // must NOT de-escalate
		{"request_changes", nil, "REQUEST_CHANGES"},
		{"request_changes", low, "REQUEST_CHANGES"},
		{"request_changes", med, "REQUEST_CHANGES"},
		{"request_changes", high, "REQUEST_CHANGES"},
	}
	for _, tc := range cases {
		fs := &Findings{Summary: summaryOf("s"), Disposition: tc.disposition, Findings: tc.findings}
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
	fs := &Findings{Summary: summaryOf("BLOCK: architectural"), Disposition: "request_changes"}
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
	fs := Consolidate(results, "sha", nil)
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
	fs := Consolidate(results, "sha", nil)
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
	}, "deadbeef", nil)

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
	// not improvise. Every other value in those examples is a self-describing
	// placeholder, so an actionable `disposition` would be copied verbatim by a
	// clean pass — posting a blocking review with zero findings, which nothing
	// downstream can correct (the floor only escalates) and which the Reviewer
	// cannot clear itself (gh pr review and both GraphQL review mutations are
	// tap-guard-blocked).
	//
	// Two things this guard gets right that a naive version does not:
	//
	//  1. It is bound to the JSON key/value shape, not to a fence label. An
	//     earlier version scoped the scan to ```json fenced blocks; that missed
	//     the same literal under a relabelled (```), differently-labelled
	//     (```JSON) or unfenced/indented example, and — because nothing
	//     asserted a block had actually been found — passed green while
	//     inspecting nothing. Scanning the whole body for the key/value pattern
	//     and requiring at least one match removes both blind spots: the value
	//     is caught regardless of its surrounding fence, and a structural
	//     regression that stops matching entirely is itself a test failure
	//     rather than a silent skip.
	//  2. It normalizes the value the way dispositionRank does. A byte-exact
	//     blacklist is spelling-sensitive while the parser lowercases and trims,
	//     so `"Comment"` or `" comment "` would re-seed an actionable value and
	//     leave the suite green.
	for _, path := range []string{
		"prompt/execution.md.tmpl",
		"../templates/roles/reviewer.md.tmpl",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		values := dispositionValues(string(data))
		if len(values) == 0 {
			t.Errorf("%s does not contain a `disposition` example — the escalation path needs a producer", path)
			continue
		}
		for _, v := range values {
			if dispositionRank(v) != 0 {
				t.Errorf("%s schema sets disposition to the actionable value %q; "+
					"a pass copying the schema would post a false verdict — use a "+
					"self-describing placeholder", path, v)
			}
		}
	}
}

// dispositionValuePattern matches every `"disposition": "<value>"` occurrence
// regardless of surrounding fence label, indentation, or whether it is fenced
// at all — the earlier ```json-scoped scan went silently vacuous under a
// fence rename because nothing asserted it had matched anything.
var dispositionValuePattern = regexp.MustCompile(`"disposition"\s*:\s*"([^"]*)"`)

// dispositionValues extracts every `disposition` value from s via
// dispositionValuePattern.
func dispositionValues(s string) []string {
	matches := dispositionValuePattern.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

func TestSchemaGuard_CatchesCaseAndSpacingVariants(t *testing.T) {
	// The guard itself needs a guard: the round-3 defect was a literal in the
	// schema, and a spelling-sensitive check would have let the same defect back
	// in under a different casing.
	for _, block := range []string{
		`{"disposition": "comment"}`,
		`{"disposition": "Comment"}`,
		`{"disposition":"REQUEST_CHANGES"}`,
		`{"disposition":  " request_changes "}`,
	} {
		// Extract with the GUARD'S OWN function, not a parallel one. An earlier
		// version used a hand-rolled scanner here, so the two tolerances were
		// independent: the scanner found the colon by hand while the guard's
		// tolerance lives entirely in its regexp's \s* groups. Tightening the
		// pattern to `"disposition":\s*"..."` would then make `"disposition" :
		// "request_changes"` invisible to the real guard while this test stayed
		// green — the guard's variant-tolerance reported as covered when it was
		// not.
		values := dispositionValues(block)
		if len(values) != 1 {
			t.Fatalf("dispositionValues(%s) = %v, want exactly one value — the guard's own "+
				"extractor must see this variant", block, values)
		}
		if dispositionRank(values[0]) == 0 {
			t.Errorf("%s: value %q must be recognized as actionable", block, values[0])
		}
	}
	// The shipped placeholder must NOT trip it. Read it out of the real
	// template via the same extraction TestSchemaExamplesDoNotSeedAnActionableDisposition
	// uses, rather than retyping it here — a prior version hardcoded a string
	// ("<omit if you completed the pass; REQUIRED if truncated>") that appears
	// nowhere in the repo, so this half of the guard exercised a placeholder
	// that isn't shipped instead of the one that is.
	data, err := os.ReadFile("prompt/execution.md.tmpl")
	if err != nil {
		t.Fatalf("reading prompt/execution.md.tmpl: %v", err)
	}
	values := dispositionValues(string(data))
	if len(values) == 0 {
		t.Fatal("prompt/execution.md.tmpl has no `disposition` example to check")
	}
	if dispositionRank(values[0]) != 0 {
		t.Errorf("the shipped placeholder %q must not be actionable", values[0])
	}
}

func TestDispositionError_SteersTheRepairAwayFromAFalseBlock(t *testing.T) {
	// This string is the only error-recovery guidance a perspective pass gets:
	// it is printed, the pass reads it, and it repairs its output from that text
	// alone. So the ORDERING is behavior, not prose. A conventional
	// "invalid disposition %q (want request_changes or comment)" enumerates only
	// the two accepted values and steers the repair toward picking one —
	// reproducing the false block this function was written to prevent. Omission
	// has to come first.
	msg := dispositionError("bogus")
	omit := strings.Index(msg, "omit")
	if omit < 0 {
		t.Fatalf("message must offer omission as the repair: %q", msg)
	}
	for _, v := range []string{"request_changes", "comment"} {
		if at := strings.Index(msg, v); at >= 0 && at < omit {
			t.Errorf("message names %q before offering omission, which steers the repair "+
				"toward a false block: %q", v, msg)
		}
	}

	// A literally-copied schema placeholder gets named as such, so the pass
	// repairs by dropping the field rather than by guessing at a value. The
	// generic message would send it looking for a real disposition it never
	// meant to set.
	ph := dispositionError("<optional; omit unless escalating — request_changes|comment>")
	if !strings.Contains(ph, "PLACEHOLDER") {
		t.Errorf("a <...> value must be named as the copied placeholder: %q", ph)
	}

	// Both surface through the two callers that hand the string to an agent.
	if _, err := ParsePerspectiveResult([]byte(`{"perspective":"p","verdict":"v","disposition":"bogus"}`)); err == nil {
		t.Error("ParsePerspectiveResult must reject an invalid disposition")
	} else if !strings.Contains(err.Error(), "omit") {
		t.Errorf("ParsePerspectiveResult must surface the recovery guidance: %v", err)
	}
	if _, err := ParseFindings([]byte(`{"summary":` + okSummary + `,"disposition":"bogus","findings":[]}`)); err == nil {
		t.Error("ParseFindings must reject an invalid disposition")
	} else if !strings.Contains(err.Error(), "omit") {
		t.Errorf("ParseFindings must surface the recovery guidance: %v", err)
	}
}

// COMMENT must never arise from severity alone. A PR carrying only medium
// findings previously derived COMMENT: it was neither approved nor blocked, so
// the fix loop had nothing to clear and a human had nothing to act on, while
// the medium threads already imposed the work. Severity now answers exactly one
// question — does this block the merge? — and only a high finding says yes.
func TestSeverityEvent_NeverYieldsComment(t *testing.T) {
	for _, priorities := range [][]string{
		nil,
		{"medium"},
		{"low", "medium", "medium"},
		{""},       // empty defaults to medium in normalizeFinding
		{"bogus"},  // ParseFindings rejects this; built in code it must not block
		{"MEDIUM"}, // casing must not change the verdict
	} {
		fs := &Findings{Summary: summaryOf("s")}
		for _, p := range priorities {
			fs.Findings = append(fs.Findings, Finding{Path: "a.go", Line: 1, Priority: p, Title: "t"})
		}
		if got := fs.SeverityEvent(); got != "APPROVE" {
			t.Errorf("priorities=%v: SeverityEvent = %q, want APPROVE", priorities, got)
		}
	}

	// … and a high finding still blocks, whatever else is present.
	fs := &Findings{Summary: summaryOf("s"), Findings: []Finding{
		{Path: "a.go", Line: 1, Priority: "medium", Title: "t"},
		{Path: "a.go", Line: 2, Priority: "high", Title: "t"},
	}}
	if got := fs.SeverityEvent(); got != "REQUEST_CHANGES" {
		t.Errorf("SeverityEvent = %q, want REQUEST_CHANGES", got)
	}
}

// COMMENT stays reachable, but only as an explicit escalation: a pass with an
// unanchorable concern — or one that cut itself short — can still keep a clean
// tally from reading as an endorsement.
func TestReviewEvent_CommentRemainsAnExplicitEscalation(t *testing.T) {
	med := []Finding{{Path: "a.go", Line: 1, Priority: "medium", Title: "t"}}

	if got := (&Findings{Summary: summaryOf("s"), Findings: med}).ReviewEvent(); got != "APPROVE" {
		t.Errorf("medium only: ReviewEvent = %q, want APPROVE", got)
	}
	if got := (&Findings{Summary: summaryOf("s"), Disposition: "comment", Findings: med}).ReviewEvent(); got != "COMMENT" {
		t.Errorf("medium + disposition=comment: ReviewEvent = %q, want COMMENT", got)
	}
	high := []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}}
	for _, d := range []string{"", "comment", "request_changes"} {
		fs := &Findings{Summary: summaryOf("s"), Disposition: d, Findings: high}
		if got := fs.ReviewEvent(); got != "REQUEST_CHANGES" {
			t.Errorf("high + disposition=%q: ReviewEvent = %q, want REQUEST_CHANGES", d, got)
		}
	}
}
