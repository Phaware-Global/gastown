package reviewer

import (
	"strings"
	"testing"
	"time"
)

func hist(counts ...int) []RoundRecord {
	out := make([]RoundRecord, 0, len(counts))
	for i, c := range counts {
		out = append(out, RoundRecord{Round: i + 1, BlockingThreads: c})
	}
	return out
}

func TestAssess_NoHistoryOrConverged(t *testing.T) {
	if d := Assess(ConvergenceInput{}); d.Triggered() {
		t.Errorf("empty history triggered: %+v", d)
	}
	// Zero blocking threads is convergence, whatever the round count.
	d := Assess(ConvergenceInput{History: hist(9, 5, 2, 0), MaxRounds: 3, Resets: 4})
	if d.Triggered() {
		t.Errorf("a drained loop must not eject: %+v", d)
	}
}

func TestAssess_StillReducingDoesNotEject(t *testing.T) {
	d := Assess(ConvergenceInput{History: hist(20, 14, 9, 5), MaxRounds: 0})
	if d.Triggered() {
		t.Errorf("a loop that is still draining must not eject: %+v", d)
	}
}

func TestAssess_StalledRounds(t *testing.T) {
	// Four rounds, none reducing: three consecutive non-reductions.
	d := Assess(ConvergenceInput{History: hist(8, 8, 9, 8)})
	if !d.Triggered() {
		t.Fatalf("three non-reducing rounds should eject: %+v", d)
	}
	if !strings.Contains(d.Reason, "no net reduction") {
		t.Errorf("reason = %q, want the stall criterion", d.Reason)
	}
	// A closing round that also opens as many threads is not progress.
	if d.BlockingThreads != 8 {
		t.Errorf("BlockingThreads = %d, want 8", d.BlockingThreads)
	}
}

func TestAssess_TwoNonReducingRoundsIsNotYetAStall(t *testing.T) {
	// A fix round that uncovers a genuine second defect looks identical to one
	// that spins; the third is where non-convergence is the better explanation.
	if d := Assess(ConvergenceInput{History: hist(5, 5, 5)}); d.Triggered() {
		t.Errorf("two non-reducing rounds should not eject yet: %+v", d)
	}
}

// The shape the dispatch caller actually builds: it escalates the moment
// review_loop_iter reaches maxIter, and reviewLoopHistory then emits exactly
// maxIter records, so latest.Round == MaxRounds. A strict > comparison made
// the cap rail silently miss this — the primary case — and no test constructed
// it, because every other case used a history one round longer than the cap.
func TestAssess_CapFiresAtTheCallerShape(t *testing.T) {
	const maxRounds = 3
	in := ConvergenceInput{
		History:   hist(5, 5, 5), // len == maxRounds, latest.Round == 3
		MaxRounds: maxRounds,
		Resets:    0,
	}
	if got := in.History[len(in.History)-1].Round; got != maxRounds {
		t.Fatalf("test setup wrong: latest.Round = %d, want %d", got, maxRounds)
	}
	d := Assess(in)
	if !d.Triggered() {
		t.Fatalf("cap rail did not fire at latest.Round == MaxRounds; the escalation "+
			"would fall back to the bare \"exceeded N iterations\" message: %+v", d)
	}
	if !strings.Contains(d.Reason, "reached the configured cap") {
		t.Errorf("reason = %q, want the cap criterion", d.Reason)
	}
}

func TestAssess_PastConfiguredCap(t *testing.T) {
	d := Assess(ConvergenceInput{History: hist(6, 5, 4, 3), MaxRounds: 3})
	if !d.Triggered() {
		t.Fatalf("round 4 past a cap of 3 should eject: %+v", d)
	}
	if !strings.Contains(d.Reason, "reached the configured cap") {
		t.Errorf("reason = %q, want the cap criterion", d.Reason)
	}
}

// The cap is evadable by clearing review_loop_iter, so a recorded reset is
// itself evidence the cap already fired once.
func TestAssess_ResetsAreEvidenceOfAPriorCap(t *testing.T) {
	d := Assess(ConvergenceInput{History: hist(4), Resets: 1, MaxRounds: 3})
	if !d.Triggered() {
		t.Fatalf("a prior reset should eject: %+v", d)
	}
	if !strings.Contains(d.Reason, "cleared 1 time(s)") {
		t.Errorf("reason = %q, want the reset criterion", d.Reason)
	}
}

func TestOutcome_ShrinkingSetApprovesWithNotes(t *testing.T) {
	// 20 -> 3: the change is sound and has been ground down by review.
	d := Assess(ConvergenceInput{History: hist(20, 12, 6, 3), MaxRounds: 3})
	if d.Outcome != EjectApproveWithNotes {
		t.Errorf("outcome = %q, want approve_with_notes", d.Outcome)
	}
}

func TestOutcome_FlatSetRecommendsDecompose(t *testing.T) {
	// The surface keeps producing defects — the shape of a PR carrying several
	// logical slices. graphql-api #120 was eventually split by hand into two.
	d := Assess(ConvergenceInput{History: hist(9, 10, 9, 11), MaxRounds: 3})
	if d.Outcome != EjectDecompose {
		t.Errorf("outcome = %q, want decompose", d.Outcome)
	}
}

func TestDescribe_PresentsAChoice(t *testing.T) {
	d := Assess(ConvergenceInput{History: hist(9, 10, 9, 11), MaxRounds: 3})
	got := d.Describe(120)

	for _, want := range []string{
		"PR #120",
		"not converging",
		"DECOMPOSE",
		"recommendation from the round history, not a merge decision",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("description missing %q:\n%s", want, got)
		}
	}
}

func TestDescribe_EmptyWhenNotTriggered(t *testing.T) {
	if got := (EjectDecision{}).Describe(1); got != "" {
		t.Errorf("untriggered decision described itself: %q", got)
	}
}

// The rail this file previously shipped without a caller able to feed it. These
// pin both halves: that a known age past the threshold ejects, and that an
// unknown age (zero) does NOT — the failure mode that would eject every PR.
func TestAssess_AgeRail(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"unknown age does not eject", 0, false},
		{"fresh PR does not eject", 2 * 24 * time.Hour, false},
		{"exactly at the threshold does not eject", MaxPRAge, false},
		{"past the threshold ejects", MaxPRAge + time.Hour, true},
		{"two weeks ejects", 14 * 24 * time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// One round, still blocking: no other rail can fire here, so the
			// result isolates the age rail.
			d := Assess(ConvergenceInput{History: hist(3), Age: tt.age})
			if d.Triggered() != tt.want {
				t.Errorf("age %v: Triggered() = %v, want %v (%+v)", tt.age, d.Triggered(), tt.want, d)
			}
			if tt.want && !strings.Contains(d.Reason, "blocking thread(s) still unresolved") {
				t.Errorf("reason = %q, want the age criterion", d.Reason)
			}
		})
	}
}

// A zero age must never read as "opened at the epoch". That inversion would
// make every PR infinitely old and eject all of them.
func TestAssess_UnknownAgeNeverEjectsAlone(t *testing.T) {
	d := Assess(ConvergenceInput{History: hist(4, 3, 2), Age: 0})
	if d.Triggered() {
		t.Errorf("unknown age ejected a still-reducing loop: %+v", d)
	}
}

// A drained PR is converged however old it is.
func TestAssess_OldButDrainedDoesNotEject(t *testing.T) {
	d := Assess(ConvergenceInput{History: hist(9, 4, 0), Age: 30 * 24 * time.Hour})
	if d.Triggered() {
		t.Errorf("an old PR with zero blocking threads must not eject: %+v", d)
	}
}

func TestFormatAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Hour, "5 hours"},
		{30 * time.Hour, "1 day"},
		{14 * 24 * time.Hour, "14 days"},
	}
	for _, c := range cases {
		if got := formatAge(c.d); got != c.want {
			t.Errorf("formatAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
