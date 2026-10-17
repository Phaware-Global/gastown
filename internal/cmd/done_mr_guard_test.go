package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

func guardEnv(prs []git.OpenPR, prsErr error, ranges map[string][]string) mrGuardEnv {
	return mrGuardEnv{
		OpenPRs: func() ([]git.OpenPR, error) { return prs, prsErr },
		RangePatchIDs: func(base, head string) ([]string, error) {
			if c, ok := ranges[head]; ok {
				return c, nil
			}
			return nil, errors.New("unknown ref " + head)
		},
	}
}

func TestCheckMRSubmitGuards(t *testing.T) {
	pr113 := git.OpenPR{Number: 113, HeadRefName: "polecat/furiosa/hga-kep", BaseRefName: "develop", IsDraft: true}
	tests := []struct {
		name     string
		branch   string
		target   string
		env      mrGuardEnv
		wantErr  string
		wantWarn bool
	}{
		{
			name: "target equals source", branch: "feature/x", target: "feature/x",
			env: guardEnv(nil, nil, nil), wantErr: "equals the source branch",
		},
		{
			name: "own PR is draft", branch: "feature/x", target: "develop",
			env:     guardEnv([]git.OpenPR{{Number: 7, HeadRefName: "feature/x", BaseRefName: "develop", IsDraft: true}}, nil, nil),
			wantErr: "draft",
		},
		{
			name: "target differs from own PR base", branch: "split/b", target: "develop",
			env:     guardEnv([]git.OpenPR{{Number: 8, HeadRefName: "split/b", BaseRefName: "split/a"}}, nil, nil),
			wantErr: "does not match PR #8's base",
		},
		{
			name: "copy of a draft PR branch", branch: "fix/on-pr113", target: "develop",
			env: guardEnv([]git.OpenPR{pr113}, nil, map[string][]string{
				"HEAD":                           {"mine", "p1", "p2"},
				"origin/polecat/furiosa/hga-kep": {"p1", "p2"},
			}),
			wantErr: "PR #113 (polecat/furiosa/hga-kep, DRAFT): 2 shared patch",
		},
		{
			name: "fork PR with same head name does not veto", branch: "polecat/x/gt-1", target: "develop",
			env: guardEnv([]git.OpenPR{{Number: 9, HeadRefName: "polecat/x/gt-1", BaseRefName: "other", IsDraft: true, IsCrossRepository: true}}, nil,
				map[string][]string{"HEAD": {"n1"}}),
		},
		{
			name: "fork PR carrying the same patches does not veto", branch: "polecat/x/gt-1", target: "develop",
			env: guardEnv([]git.OpenPR{{Number: 10, HeadRefName: "polecat/y/gt-2", BaseRefName: "develop", IsCrossRepository: true}}, nil,
				map[string][]string{"HEAD": {"p1"}, "origin/polecat/y/gt-2": {"p1"}}),
		},
		{
			name: "fresh branch, unrelated open PR", branch: "polecat/x/gt-1", target: "main",
			env: guardEnv([]git.OpenPR{pr113}, nil, map[string][]string{
				"HEAD":                           {"n1"},
				"origin/polecat/furiosa/hga-kep": {"p1", "p2"},
			}),
		},
		{
			name: "own non-draft PR on matching base", branch: "feature/x", target: "develop",
			env: guardEnv([]git.OpenPR{{Number: 7, HeadRefName: "feature/x", BaseRefName: "develop"}}, nil,
				map[string][]string{"HEAD": {"n1"}}),
		},
		{
			name: "gh unavailable fails open", branch: "feature/x", target: "develop",
			env: guardEnv(nil, errors.New("gh: not logged in"), nil), wantWarn: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warns, err := checkMRSubmitGuards(tt.env, tt.branch, tt.target)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if tt.wantWarn != (len(warns) > 0) {
				t.Fatalf("warnings = %v, wantWarn %v", warns, tt.wantWarn)
			}
		})
	}
}
