package git

import (
	"reflect"
	"testing"
)

// TestBuildRequestReviewArgv pins reviewer normalization: every reviewer
// goes through `gh pr edit --add-reviewer` (no vendor-bot trigger-comment
// special-casing — the in-town Reviewer role, merge_queue.reviewer_local,
// is the sanctioned automated-review path). Empty/whitespace entries are
// dropped so we never emit "a,,b" to gh, which would fail.
//
// This test covers normalization only — it does not invoke `gh`.
func TestBuildRequestReviewArgv(t *testing.T) {
	tests := []struct {
		name         string
		reviewers    []string
		wantEditArgv []string
	}{
		{
			name:         "empty reviewers",
			reviewers:    nil,
			wantEditArgv: nil,
		},
		{
			name:         "single user — edit path",
			reviewers:    []string{"kevinpjones"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "kevinpjones"},
		},
		{
			name:         "multiple users — CSV edit path",
			reviewers:    []string{"alice", "bob"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		{
			name:         "team — edit path",
			reviewers:    []string{"@myorg/reviewers"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "@myorg/reviewers"},
		},
		// Whitespace normalization — config fields often carry trailing
		// newlines or stray spaces; entries are TrimSpace'd before joining.
		{
			name:         "reviewer with surrounding whitespace — trimmed",
			reviewers:    []string{"  alice  "},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "alice"},
		},
		{
			name:         "reviewer surrounded by tabs/newlines — trimmed",
			reviewers:    []string{"\talice\n"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "alice"},
		},
		// Empty-after-trim dropping — never emit "a,,b" to gh, which would fail.
		{
			name:         "empty string in list — dropped, no blank reviewer emitted",
			reviewers:    []string{"alice", "", "bob"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		{
			name:         "whitespace-only string in list — dropped",
			reviewers:    []string{"alice", "   ", "bob"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		{
			name:         "all entries empty/whitespace — nil argv",
			reviewers:    []string{"", "  ", "\t"},
			wantEditArgv: nil,
		},
		{
			name:         "users alongside empty entries — routes cleanly",
			reviewers:    []string{"", "  alice  ", "\t", "bob"},
			wantEditArgv: []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotEdit := buildRequestReviewArgv(42, tc.reviewers)
			if !reflect.DeepEqual(gotEdit, tc.wantEditArgv) {
				t.Errorf("editArgv = %v; want %v", gotEdit, tc.wantEditArgv)
			}
		})
	}
}
