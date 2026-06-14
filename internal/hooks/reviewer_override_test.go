package hooks

import (
	"strings"
	"testing"
)

// TestReviewerOverrideBlocksWriteSurfaces asserts the Reviewer role's tap-guard
// (P23-2376) blocks every dangerous write surface: posting raw reviews,
// merging, pushing, driving the refinery, and resolving threads. The Reviewer's
// only sanctioned write path is `gt reviewer post`.
func TestReviewerOverrideBlocksWriteSurfaces(t *testing.T) {
	overrides := DefaultOverrides()
	rev, ok := overrides["reviewer"]
	if !ok {
		t.Fatal("DefaultOverrides() has no \"reviewer\" entry")
	}
	if len(rev.PreToolUse) == 0 {
		t.Fatal("reviewer override has no PreToolUse guards")
	}

	// Each dangerous command must be matched by some PreToolUse entry whose
	// hook blocks with a non-zero exit.
	wantBlocked := []string{
		"gh pr review",
		"gh pr merge",
		"git push",
		"gt refinery pr",
		"resolveReviewThread",
	}
	for _, needle := range wantBlocked {
		if !matcherCovers(rev.PreToolUse, needle) {
			t.Errorf("reviewer override does not guard %q", needle)
		}
	}

	// Every guard must actually block (exit 2), not just warn.
	for _, entry := range rev.PreToolUse {
		for _, h := range entry.Hooks {
			if !strings.Contains(h.Command, "exit 2") {
				t.Errorf("guard %q does not block (missing 'exit 2'): %q", entry.Matcher, h.Command)
			}
		}
	}
}

// TestReviewerOverrideApplicableViaRigRole confirms the override key resolves
// for a rig-scoped reviewer target (e.g. "gastown/reviewer").
func TestReviewerOverrideApplicableViaRigRole(t *testing.T) {
	got := GetApplicableOverrides("gastown/reviewer")
	found := false
	for _, k := range got {
		if k == "reviewer" {
			found = true
		}
	}
	if !found {
		t.Errorf("GetApplicableOverrides(gastown/reviewer) = %v, missing \"reviewer\"", got)
	}
}

func matcherCovers(entries []HookEntry, needle string) bool {
	for _, e := range entries {
		if strings.Contains(e.Matcher, needle) {
			return true
		}
	}
	return false
}
