package beads

import "testing"

func TestHoldReason(t *testing.T) {
	cases := []struct {
		name  string
		issue *Issue
		want  string
	}{
		{"nil", nil, ""},
		{"open, no labels", &Issue{Status: "open"}, ""},
		{"unrelated labels", &Issue{Labels: []string{"gt:task", "holdout-test"}}, ""},
		{"hold label", &Issue{Labels: []string{"gt:merge-request", "hold"}}, "labeled hold"},
		{"held label, any case", &Issue{Labels: []string{"Held"}}, "labeled held"},
		{"on-hold label", &Issue{Labels: []string{"on-hold"}}, "labeled on-hold"},
		{"deferred status", &Issue{Status: "deferred"}, "status deferred"},
		{"closed", &Issue{Status: "closed"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HoldReason(c.issue); got != c.want {
				t.Errorf("HoldReason = %q, want %q", got, c.want)
			}
		})
	}
}
