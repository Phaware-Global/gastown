package polecat

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

type fakeActiveMRReader struct {
	issues map[string]*beads.Issue
	errs   map[string]error
}

func (f fakeActiveMRReader) Show(issueID string) (*beads.Issue, error) {
	if err := f.errs[issueID]; err != nil {
		return nil, err
	}
	issue, ok := f.issues[issueID]
	if !ok {
		return nil, beads.ErrNotFound
	}
	return issue, nil
}

func TestAssessActiveMR(t *testing.T) {
	reader := fakeActiveMRReader{issues: map[string]*beads.Issue{
		"mr-open":        &beads.Issue{ID: "mr-open", Status: "open"},
		"mr-closed":      &beads.Issue{ID: "mr-closed", Status: "closed"},
		"mr-with-source": &beads.Issue{ID: "mr-with-source", Status: "closed", Description: "source_issue: gt-closed\n"},
		"gt-closed":      &beads.Issue{ID: "gt-closed", Status: "closed"},
		"gt-open":        &beads.Issue{ID: "gt-open", Status: "open"},
	}}

	tests := []struct {
		name       string
		reader     IssueReader
		input      ActiveMRInput
		wantPend   bool
		wantSource string
	}{
		{name: "empty active MR is not pending", reader: reader, input: ActiveMRInput{}, wantPend: false},
		{name: "open MR is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-open", SourceIssueHint: "gt-closed"}, wantPend: true},
		{name: "closed MR with terminal source is stale", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed"}, wantPend: false, wantSource: "gt-closed"},
		{name: "closed MR with unknown source is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed"}, wantPend: true},
		{name: "closed MR with open source is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-open"}, wantPend: true, wantSource: "gt-open"},
		{name: "missing MR with terminal source is stale", reader: reader, input: ActiveMRInput{ActiveMR: "mr-missing", SourceIssueHint: "gt-closed"}, wantPend: false, wantSource: "gt-closed"},
		{name: "missing MR with missing source is pending", reader: reader, input: ActiveMRInput{ActiveMR: "mr-missing", SourceIssueHint: "gt-missing"}, wantPend: true, wantSource: "gt-missing"},
		{name: "terminal MR source wins from description", reader: reader, input: ActiveMRInput{ActiveMR: "mr-with-source"}, wantPend: false, wantSource: "gt-closed"},
		{name: "nil reader fails closed", reader: nil, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed"}, wantPend: true},
		{name: "git unsafe fails closed when required", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed", RequireGitSafe: true}, wantPend: true, wantSource: "gt-closed"},
		{name: "git safe permits stale when required", reader: reader, input: ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-closed", RequireGitSafe: true, GitSafe: true}, wantPend: false, wantSource: "gt-closed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssessActiveMR(tt.reader, tt.input)
			if got.Pending != tt.wantPend {
				t.Fatalf("Pending = %v, want %v (reason %q)", got.Pending, tt.wantPend, got.Reason)
			}
			if tt.wantSource != "" && got.SourceIssue != tt.wantSource {
				t.Fatalf("SourceIssue = %q, want %q", got.SourceIssue, tt.wantSource)
			}
		})
	}
}

func TestAssessActiveMRLookupErrorsFailClosed(t *testing.T) {
	reader := fakeActiveMRReader{
		issues: map[string]*beads.Issue{"gt-closed": &beads.Issue{ID: "gt-closed", Status: "closed"}},
		errs:   map[string]error{"mr-error": errors.New("bd exploded"), "gt-error": errors.New("bd exploded")},
	}

	if got := AssessActiveMR(reader, ActiveMRInput{ActiveMR: "mr-error", SourceIssueHint: "gt-closed"}); !got.Pending {
		t.Fatalf("MR lookup error Pending = false, want true")
	}
	reader.issues["mr-closed"] = &beads.Issue{ID: "mr-closed", Status: "closed"}
	if got := AssessActiveMR(reader, ActiveMRInput{ActiveMR: "mr-closed", SourceIssueHint: "gt-error"}); !got.Pending {
		t.Fatalf("source lookup error Pending = false, want true")
	}
}

// TestAssessActiveMR_VanishedMRWithNoSourceIsGitDecided covers the permanent
// wedge seen on gastown/capable: active_mr named a wisp that the reaper had
// purged, so no source issue could be recovered and terminalSourceIssue could
// never return terminal. Pending stayed true on every evaluation, and only the
// refinery clears active_mr — which it never does for an MR that no longer
// exists. The polecat was undispatchable with nothing at risk.
func TestAssessActiveMR_VanishedMRWithNoSourceIsGitDecided(t *testing.T) {
	reader := fakeActiveMRReader{issues: map[string]*beads.Issue{}}

	t.Run("git safe clears", func(t *testing.T) {
		got := AssessActiveMR(reader, ActiveMRInput{
			ActiveMR:       "gt-wisp-gone",
			RequireGitSafe: true,
			GitSafe:        true,
		})
		if got.Pending {
			t.Errorf("Pending = true (%s), want false: the MR is gone and git holds no work at risk", got.Reason)
		}
		if !got.Stale {
			t.Error("Stale = false, want true for a vanished MR")
		}
	})

	t.Run("git unsafe still blocks", func(t *testing.T) {
		got := AssessActiveMR(reader, ActiveMRInput{
			ActiveMR:       "gt-wisp-gone",
			RequireGitSafe: true,
			GitSafe:        false,
		})
		if !got.Pending {
			t.Error("Pending = false, want true: git still holds work at risk")
		}
	})

	t.Run("no git evidence still blocks", func(t *testing.T) {
		got := AssessActiveMR(reader, ActiveMRInput{
			ActiveMR:       "gt-wisp-gone",
			RequireGitSafe: false,
		})
		if !got.Pending {
			t.Error("Pending = false, want true: callers supplying no git evidence keep the fail-closed behavior")
		}
	})
}

// TestAssessActiveMR_VanishedMRWithKnownSourceStillChecksSource verifies the
// git-decided path is confined to the unfalsifiable case. When the source issue
// IS recoverable from the hint, it must still gate the decision — a git-clean
// worktree must not clear a still-open source issue.
func TestAssessActiveMR_VanishedMRWithKnownSourceStillChecksSource(t *testing.T) {
	reader := fakeActiveMRReader{issues: map[string]*beads.Issue{
		"gt-open": {ID: "gt-open", Status: "open"},
	}}

	got := AssessActiveMR(reader, ActiveMRInput{
		ActiveMR:        "gt-wisp-gone",
		SourceIssueHint: "gt-open",
		RequireGitSafe:  true,
		GitSafe:         true,
	})
	if !got.Pending {
		t.Error("Pending = false, want true: the source issue is known and still open")
	}
}
