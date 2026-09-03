package cmd

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestClassifyUnsavedPaths pins the PR #184 finding: the "N new untracked
// file(s) ... commit them yourself" warning was sourced from
// NonRuntimePaths(), which also contains modified TRACKED files that gt done
// itself deliberately excluded (the polecat overlay CLAUDE.md) — telling the
// operator to commit the one file the exclusion exists to keep out of the PR.
func TestClassifyUnsavedPaths(t *testing.T) {
	status := &git.UncommittedWorkStatus{
		ModifiedFiles:  []string{"CLAUDE.md", ".beads/state.json"},
		UntrackedFiles: []string{"new_feature.go", ".claude/settings.json"},
	}

	newUntracked, excluded := classifyUnsavedPaths(status, []string{"CLAUDE.md"})

	if want := []string{"new_feature.go"}; !reflect.DeepEqual(newUntracked, want) {
		t.Errorf("newUntracked = %v, want %v — runtime artifacts and excluded tracked files must not be reported as new untracked work", newUntracked, want)
	}
	if want := []string{"CLAUDE.md"}; !reflect.DeepEqual(excluded, want) {
		t.Errorf("excluded = %v, want %v — the deliberately-excluded overlay must be reported as excluded, never as something to commit", excluded, want)
	}

	// The overlay-only case that produced the wrong message: modified
	// excluded CLAUDE.md is the sole non-runtime change.
	overlayOnly := &git.UncommittedWorkStatus{ModifiedFiles: []string{"CLAUDE.md"}}
	newUntracked, excluded = classifyUnsavedPaths(overlayOnly, []string{"CLAUDE.md"})
	if len(newUntracked) != 0 {
		t.Errorf("newUntracked = %v, want none — a tracked, deliberately-excluded file is not a new untracked file", newUntracked)
	}
	if want := []string{"CLAUDE.md"}; !reflect.DeepEqual(excluded, want) {
		t.Errorf("excluded = %v, want %v", excluded, want)
	}

	if nu, ex := classifyUnsavedPaths(nil, nil); nu != nil || ex != nil {
		t.Errorf("nil status: got %v, %v, want nil, nil", nu, ex)
	}
}
