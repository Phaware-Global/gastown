package formula

import (
	"strings"
	"testing"
)

// The dog-health-check step must use the timing fields `gt dog` computes and
// never ask the agent to do date arithmetic: a hand-computed duration came out
// wrong by exactly the timestamp's UTC offset and filed a false death warrant.
func TestDeaconPatrolDogHealthUsesComputedTiming(t *testing.T) {
	content, err := GetEmbeddedFormulaContent("mol-deacon-patrol")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	body := string(content)

	start := strings.Index(body, `id = "dog-health-check"`)
	if start < 0 {
		t.Fatal("dog-health-check step not found")
	}
	step := body[start:]
	if end := strings.Index(step[1:], "[[steps]]"); end >= 0 {
		step = step[:end+1]
	}

	for _, want := range []string{"work_elapsed_seconds", "timeout_seconds", "over_timeout_ratio"} {
		if !strings.Contains(step, want) {
			t.Errorf("dog-health-check does not reference %s", want)
		}
	}
	for _, banned := range []string{"now - work_started_at", "<duration>"} {
		if strings.Contains(step, banned) {
			t.Errorf("dog-health-check still asks the agent to compute a duration: %q", banned)
		}
	}
}
