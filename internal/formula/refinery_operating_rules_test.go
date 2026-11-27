package formula

import (
	"strings"
	"testing"
)

func refineryStepDescription(t *testing.T, id string) string {
	t.Helper()
	data, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing refinery formula: %v", err)
	}
	for _, step := range f.Steps {
		if step.ID == id {
			return step.Description
		}
	}
	t.Fatalf("step %q not found", id)
	return ""
}

// Unbounded full-suite runs load the host enough to slow the shared beads server.
func TestRefineryPatrolBoundsFullSuiteTestRuns(t *testing.T) {
	desc := refineryStepDescription(t, "run-tests")
	for _, want := range []string{
		"go test -p 2 ./...",
		"one package at a time",
		"Never run more than one full test suite at once",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("run-tests step missing %q", want)
		}
	}
}

// The mayor may be hooking a fix bead of their own, so the patrol gets an answer
// before it dispatches a second one.
func TestRefineryPatrolAsksBeforeReviewFixDispatch(t *testing.T) {
	desc := refineryStepDescription(t, "merge-push")
	ask := strings.Index(desc, "Ask the mayor before dispatching")
	dispatch := strings.Index(desc, "{{ cmd }} refinery pr dispatch-review-fix $PR --mr $MR\nDISPATCH_RC")
	if ask < 0 || dispatch < 0 {
		t.Fatalf("ask-first rule (at %d) and dispatch call (at %d) must both be present", ask, dispatch)
	}
	if ask > dispatch {
		t.Error("the ask-first rule must come before the dispatch call")
	}
	rule := desc[ask:dispatch]
	for _, want := range []string{
		"Wait for the answer",
		"Silence is not a yes",
		"a nudge can sit unsent",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("ask-first rule missing %q", want)
		}
	}
}
