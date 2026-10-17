package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

func stubPolecatWorkStatus(t *testing.T, status *git.UncommittedWorkStatus, err error) *int {
	t.Helper()
	calls := 0
	orig := polecatWorkStatusFn
	polecatWorkStatusFn = func(string) (*git.UncommittedWorkStatus, error) {
		calls++
		return status, err
	}
	t.Cleanup(func() { polecatWorkStatusFn = orig })
	return &calls
}

func makePolecatClone(t *testing.T, townRoot, rig, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, rig, "polecats", name, rig), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReassignPreservesWork(t *testing.T) {
	const assignee = "gastown/polecats/toast"

	tests := []struct {
		name      string
		assignee  string
		makeClone bool
		status    *git.UncommittedWorkStatus
		statusErr error
		wantErr   string
		wantCalls int
	}{
		{name: "non-polecat holder passes", assignee: "gastown/crew/max", wantCalls: 0},
		{name: "empty holder passes", assignee: "", wantCalls: 0},
		{name: "missing worktree passes", assignee: assignee, wantCalls: 0},
		{name: "clean worktree passes", assignee: assignee, makeClone: true, status: &git.UncommittedWorkStatus{}, wantCalls: 1},
		{
			name: "runtime-only changes pass", assignee: assignee, makeClone: true, wantCalls: 1,
			status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".claude/settings.json"}},
		},
		{
			name: "uncommitted edits refuse", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "unpreserved work",
			status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{"internal/cmd/sling.go"}},
		},
		{
			name: "untracked source refuses", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "unpreserved work",
			status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{"internal/cmd/new.go"}},
		},
		{
			name: "stash refuses", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "1 stash(es)",
			status: &git.UncommittedWorkStatus{StashCount: 1},
		},
		{
			name: "unpushed commits refuse", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "2 unpushed commit(s)",
			status: &git.UncommittedWorkStatus{UnpushedCommits: 2},
		},
		{
			name: "status error fails closed", assignee: assignee, makeClone: true, wantCalls: 1, wantErr: "cannot verify",
			statusErr: errors.New("git exploded"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if tt.makeClone {
				makePolecatClone(t, townRoot, "gastown", "toast")
			}
			calls := stubPolecatWorkStatus(t, tt.status, tt.statusErr)

			err := checkReassignPreservesWork(townRoot, tt.assignee)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if *calls != tt.wantCalls {
				t.Errorf("status checks = %d, want %d", *calls, tt.wantCalls)
			}
		})
	}
}
