package cmd

import (
	"path/filepath"
	"testing"
)

// TestIsPolecatSession covers PR #234 round 1 finding B (RoleUnknown must not
// exempt the guards) and round 3 (discussion_r4050325633): a KNOWN
// non-polecat role detected from cwd must not exempt the guards either when
// GT_ROLE is unset and GT_POLECAT is set — that combination means role
// detection is running for a polecat whose cwd reconstruction failed, not a
// genuine coordinator session (coordinators always carry GT_ROLE).
func TestIsPolecatSession(t *testing.T) {
	townRoot := "/gt"

	tests := []struct {
		name       string
		cwd        string
		envRole    string // GT_ROLE; "" leaves it unset
		envPolecat string // GT_POLECAT; "" leaves it unset
		want       bool
	}{
		{
			name:       "unresolved cwd (RoleUnknown), GT_ROLE unset, GT_POLECAT set",
			cwd:        townRoot,
			envPolecat: "dag",
			want:       true, // round 1 fix (finding B)
		},
		{
			name: "unresolved cwd (RoleUnknown), GT_ROLE unset, GT_POLECAT unset",
			cwd:  townRoot,
			want: false,
		},
		{
			name:       "known non-polecat cwd (RoleMayor), GT_ROLE unset, GT_POLECAT set",
			cwd:        filepath.Join(townRoot, "mayor", "rig"),
			envPolecat: "dag",
			want:       true, // round 3 fix (discussion_r4050325633): cwd reconstruction failed
		},
		{
			name: "known non-polecat cwd (RoleMayor), GT_ROLE unset, GT_POLECAT unset",
			cwd:  filepath.Join(townRoot, "mayor", "rig"),
			want: false, // genuine mayor session
		},
		{
			name:       "known polecat cwd (RolePolecat), GT_ROLE unset, GT_POLECAT unset",
			cwd:        filepath.Join(townRoot, "gastown", "polecats", "dag", "gastown"),
			envPolecat: "",
			want:       true,
		},
		{
			name:       "GT_ROLE explicitly set to crew, stale GT_POLECAT set",
			cwd:        filepath.Join(townRoot, "mayor", "rig"),
			envRole:    "gastown/crew/toast",
			envPolecat: "dag",
			want:       false, // explicit env role wins; stale GT_POLECAT must not override it
		},
		{
			name:       "GT_ROLE explicitly set to polecat",
			cwd:        townRoot,
			envRole:    "gastown/polecats/dag",
			envPolecat: "dag",
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_ROLE", tt.envRole)
			t.Setenv("GT_POLECAT", tt.envPolecat)
			t.Setenv("GT_RIG", "")
			t.Setenv("GT_CREW", "")

			got := isPolecatSession(tt.cwd, townRoot)
			if got != tt.want {
				t.Errorf("isPolecatSession(%q, %q) with GT_ROLE=%q GT_POLECAT=%q = %v, want %v",
					tt.cwd, townRoot, tt.envRole, tt.envPolecat, got, tt.want)
			}
		})
	}
}
