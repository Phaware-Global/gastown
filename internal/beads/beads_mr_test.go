package beads

import "testing"

func TestMatchesMRSourceIssue(t *testing.T) {
	tests := []struct {
		name        string
		description string
		issueID     string
		want        bool
	}{
		{
			name:        "exact match",
			description: "branch: polecat/furiosa/gt-abc@mm4heq3e\ntarget: main\nsource_issue: gt-abc\nrig: gastown\n",
			issueID:     "gt-abc",
			want:        true,
		},
		{
			name:        "no match different issue",
			description: "branch: polecat/furiosa/gt-xyz@mm4heq3e\ntarget: main\nsource_issue: gt-xyz\nrig: gastown\n",
			issueID:     "gt-abc",
			want:        false,
		},
		{
			name:        "partial ID must not match — prefix",
			description: "branch: polecat/nux/gt-abcdef@mm4heq3e\ntarget: main\nsource_issue: gt-abcdef\nrig: gastown\n",
			issueID:     "gt-abc",
			want:        false,
		},
		{
			name:        "partial ID must not match — suffix",
			description: "branch: polecat/nux/gt-abc@mm4heq3e\ntarget: main\nsource_issue: gt-abc\nrig: gastown\n",
			issueID:     "gt-abcdef",
			want:        false,
		},
		{
			name:        "match with worker field after source_issue",
			description: "branch: polecat/furiosa/la-cagb2@mm4heq3e\ntarget: main\nsource_issue: la-cagb2\nworker: polecats/furiosa\n",
			issueID:     "la-cagb2",
			want:        true,
		},
		{
			name:        "source_issue at end of description (with trailing newline)",
			description: "branch: fix/thing\nsource_issue: gt-99\n",
			issueID:     "gt-99",
			want:        true,
		},
		{
			name:        "source_issue at end without trailing newline — no match",
			description: "branch: fix/thing\nsource_issue: gt-99",
			issueID:     "gt-99",
			want:        false,
		},
		{
			name:        "empty description",
			description: "",
			issueID:     "gt-abc",
			want:        false,
		},
		{
			name:        "empty issue ID",
			description: "source_issue: gt-abc\n",
			issueID:     "",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesMRSourceIssue(tt.description, tt.issueID)
			if got != tt.want {
				t.Errorf("MatchesMRSourceIssue(%q, %q) = %v, want %v",
					tt.description, tt.issueID, got, tt.want)
			}
		})
	}
}

// TestPickMRForBranch covers the branch-match logic used to dedup MR
// beads by branch name. The old implementation used
// strings.HasPrefix(description, "branch: X\n"); the 2026-04-19
// Telegraph v1 dogfood showed that fragile to real description shapes
// (leading blank line, different field order, header comments, leading
// whitespace). The current implementation parses the structured
// `branch:` field via ParseMRFields, which is whitespace- and
// position-tolerant. Each positive case below is a description shape
// the OLD HasPrefix implementation would have missed.
func TestPickMRForBranch(t *testing.T) {
	mk := func(id, description, status string) *Issue {
		return &Issue{
			ID:          id,
			Description: description,
			Status:      status,
			Labels:      []string{"gt:merge-request"},
		}
	}

	standardDesc := "branch: polecat/foo\ntarget: main\nsource_issue: gt-xxx\n"
	leadingBlankDesc := "\nbranch: polecat/foo\ntarget: main\n"
	headerCommentDesc := "Backfilled manually to unstick PR\n\nbranch: polecat/foo\ntarget: main\n"
	reorderedDesc := "target: main\nsource_issue: gt-xxx\nbranch: polecat/foo\n"
	withExtraWhitespaceDesc := "  branch: polecat/foo  \ntarget: main\n"
	upperKeyDesc := "Branch: polecat/foo\ntarget: main\n"
	differentBranchDesc := "branch: polecat/bar\ntarget: main\n"
	noBranchDesc := "target: main\nsource_issue: gt-xxx\n"
	partialBranchDesc := "branch: polecat/foo-extra\ntarget: main\n"

	tests := []struct {
		name       string
		issues     []*Issue
		branch     string
		skipClosed bool
		wantID     string
	}{
		{
			"standard newline-separated — matches",
			[]*Issue{mk("gt-1", standardDesc, "open")},
			"polecat/foo", true, "gt-1",
		},
		{
			"leading blank line — OLD HasPrefix would miss",
			[]*Issue{mk("gt-2", leadingBlankDesc, "open")},
			"polecat/foo", true, "gt-2",
		},
		{
			"header comment before fields — OLD HasPrefix would miss",
			[]*Issue{mk("gt-3", headerCommentDesc, "open")},
			"polecat/foo", true, "gt-3",
		},
		{
			"reordered fields (branch not first) — OLD HasPrefix would miss",
			[]*Issue{mk("gt-4", reorderedDesc, "open")},
			"polecat/foo", true, "gt-4",
		},
		{
			"leading whitespace on branch line — OLD HasPrefix would miss",
			[]*Issue{mk("gt-5", withExtraWhitespaceDesc, "open")},
			"polecat/foo", true, "gt-5",
		},
		{
			"uppercase Branch key — case-insensitive match",
			[]*Issue{mk("gt-6", upperKeyDesc, "open")},
			"polecat/foo", true, "gt-6",
		},
		{
			"different branch — no match",
			[]*Issue{mk("gt-7", differentBranchDesc, "open")},
			"polecat/foo", true, "",
		},
		{
			"description lacks branch field",
			[]*Issue{mk("gt-8", noBranchDesc, "open")},
			"polecat/foo", true, "",
		},
		{
			"substring collision — polecat/foo vs polecat/foo-extra",
			[]*Issue{mk("gt-9", partialBranchDesc, "open")},
			"polecat/foo", true, "",
		},
		{
			"empty issue list",
			nil,
			"polecat/foo", true, "",
		},
		{
			"closed match excluded when skipClosed=true",
			[]*Issue{mk("gt-10", standardDesc, "closed")},
			"polecat/foo", true, "",
		},
		{
			"closed match included when skipClosed=false",
			[]*Issue{mk("gt-11", standardDesc, "closed")},
			"polecat/foo", false, "gt-11",
		},
		{
			"multiple MRs, different branches — returns the matching one",
			[]*Issue{
				mk("gt-12", differentBranchDesc, "open"),
				mk("gt-13", standardDesc, "open"),
			},
			"polecat/foo", true, "gt-13",
		},
		{
			"multiple MRs, matching one closed + matching one open — returns open when skipClosed=true",
			[]*Issue{
				mk("gt-14", standardDesc, "closed"),
				mk("gt-15", standardDesc, "open"),
			},
			"polecat/foo", true, "gt-15",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickMRForBranch(tc.issues, tc.branch, tc.skipClosed)
			switch {
			case tc.wantID == "" && got == nil:
				// ok
			case tc.wantID == "" && got != nil:
				t.Errorf("pickMRForBranch returned %q; want nil", got.ID)
			case tc.wantID != "" && got == nil:
				t.Errorf("pickMRForBranch returned nil; want %q", tc.wantID)
			case got.ID != tc.wantID:
				t.Errorf("pickMRForBranch returned %q; want %q", got.ID, tc.wantID)
			}
		})
	}
}
