package cmd

import "testing"

// TestIsPushToMain covers the push-refspec classifier for the push-main
// guard. The incident that motivated the guard used
// `git push origin FETCH_HEAD:refs/heads/main`, which doesn't look like
// "push to main" in a naïve check — the destination is buried after a
// colon in the refspec. We explicitly test that shape plus common
// variants, and keep negative cases that must pass through.
func TestIsPushToMain(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		// Positives — must be blocked
		{"bare push to main", "git push origin main", true},
		{"HEAD to main", "git push origin HEAD:main", true},
		{"HEAD to refs/heads/main", "git push origin HEAD:refs/heads/main", true},
		{"FETCH_HEAD to refs/heads/main (observed)", "git push origin FETCH_HEAD:refs/heads/main", true},
		{"branch to main", "git push origin polecat/foo:main", true},
		{"branch to refs/heads/main", "git push origin polecat/foo:refs/heads/main", true},
		{"with -f flag", "git push -f origin main", true},
		{"with --force-with-lease", "git push --force-with-lease origin main", true},
		{"leading whitespace", "   git push origin main", true},
		{"main alone (no remote)", "git push main", true},

		// Negatives — must pass through
		{"push to feature branch", "git push origin polecat/foo", false},
		{"branch:branch", "git push origin polecat/foo:polecat/foo", false},
		{"branch to non-main refspec", "git push origin polecat/foo:feat/thing", false},
		{"fetch not push", "git fetch origin main", false},
		{"echo containing push", `echo "git push origin main"`, false},
		{"push to mainline (not main)", "git push origin mainline", false},
		{"push to main/foo (not main)", "git push origin HEAD:main/foo", false},
		{"empty command", "", false},
		{"not a git command", "ls", false},
		{"git without push", "git status", false},

		// Edge: `main` as a refspec LHS, different RHS — still not a push
		// TO main. `main:polecat/foo` would only happen if someone is
		// doing something weird locally, not a main-landing push.
		{"main as source, branch as dest", "git push origin main:polecat/foo", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPushToMain(tc.cmd)
			if got != tc.want {
				t.Errorf("isPushToMain(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
