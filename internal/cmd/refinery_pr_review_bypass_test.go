package cmd

import "testing"

// TestBranchMatchesReviewBypass pins the glob matching that decides whether a
// PR's head branch opts out of the automated review loop. Patterns use
// path.Match semantics: "*" does not cross "/".
func TestBranchMatchesReviewBypass(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		branch   string
		want     bool
	}{
		{"no patterns", nil, "release/v2", false},
		{"empty branch", []string{"release/*"}, "", false},
		{"release glob matches", []string{"release/*"}, "release/v2", true},
		{"release glob matches dotted", []string{"release/*"}, "release/v2.1", true},
		{"release glob no nested cross-slash", []string{"release/*"}, "release/2024/v2", false},
		{"non-matching feature branch", []string{"release/*"}, "feature/login", false},
		{"exact branch name", []string{"production"}, "production", true},
		{"exact mismatch", []string{"production"}, "production-hotfix", false},
		{"multiple patterns, second matches", []string{"hotfix/*", "release/*"}, "release/v3", true},
		{"empty-string pattern is skipped", []string{""}, "release/v2", false},
		{"malformed pattern fails safe (no match)", []string{"release/["}, "release/[", false},
		{"malformed pattern does not block a later valid one", []string{"release/[", "release/*"}, "release/v2", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := branchMatchesReviewBypass(c.patterns, c.branch); got != c.want {
				t.Errorf("branchMatchesReviewBypass(%q, %q) = %v; want %v", c.patterns, c.branch, got, c.want)
			}
		})
	}
}
