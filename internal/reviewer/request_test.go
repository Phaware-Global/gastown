package reviewer

import (
	"strings"
	"testing"
)

func TestDefaultOrigin(t *testing.T) {
	cases := []struct {
		explicit, mr, want string
	}{
		{"", "", OriginCrew},
		{"", "gt-mr-1", OriginRefinery},
		{OriginCrew, "gt-mr-1", OriginCrew},  // explicit wins
		{OriginRefinery, "", OriginRefinery}, // explicit wins
		{"bogus", "gt-mr-1", OriginRefinery}, // invalid explicit → mr-derived
		{"bogus", "", OriginCrew},            // invalid explicit, no mr → crew
	}
	for _, c := range cases {
		if got := DefaultOrigin(c.explicit, c.mr); got != c.want {
			t.Errorf("DefaultOrigin(%q,%q) = %q, want %q", c.explicit, c.mr, got, c.want)
		}
	}
}

func TestRequestSpec_Body(t *testing.T) {
	s := RequestSpec{
		PR: 42, HeadSHA: "abc123", Branch: "polecat/x/feat",
		Round: 2, Origin: OriginRefinery, MRID: "gt-mr-9",
	}
	body := s.Body("- a.go:10 [bot] nil deref")

	for _, want := range []string{
		"REVIEW_REQUEST", "pr: 42", "head_sha: abc123",
		"branch: polecat/x/feat", "round: 2", "origin: refinery", "mr: gt-mr-9",
		"PRIOR_ROUND_THREADS", "a.go:10",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Body missing %q:\n%s", want, body)
		}
	}
}

func TestRequestSpec_Body_Round1NoPriorThreads(t *testing.T) {
	s := RequestSpec{PR: 7, HeadSHA: "deadbeef", Round: 1, MRID: ""}
	body := s.Body("")
	if strings.Contains(body, "PRIOR_ROUND_THREADS") {
		t.Errorf("round-1 body should have no prior-threads block:\n%s", body)
	}
	if !strings.Contains(body, "origin: crew") {
		t.Errorf("no MR → origin should default to crew:\n%s", body)
	}
	if strings.Contains(body, "mr:") {
		t.Errorf("no MR id → no mr line:\n%s", body)
	}
}

func TestRequestSpec_Subject_DefaultsRound(t *testing.T) {
	s := RequestSpec{PR: 5, Round: 0}
	if got := s.Subject(); got != "Review request: PR #5 (round 1)" {
		t.Errorf("Subject() = %q", got)
	}
}
