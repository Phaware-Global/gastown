package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// Failures that happen before the sync/push runs must still exit 4 with a
// message on stderr: the patrol formula handles 0/2/3/4 and nothing else.
func TestRefineryPrSyncPush_PreflightFailuresExitRefused(t *testing.T) {
	t.Setenv("GT_ROOT", "")
	t.Chdir(t.TempDir())

	cases := []struct {
		name string
		run  func(*cobra.Command, []string) error
		set  func()
	}{
		{"sync-branch missing flags", runRefineryPrSyncBranch, func() { refPrSyncBranch, refPrSyncTarget = "", "" }},
		{"sync-branch outside a workspace", runRefineryPrSyncBranch, func() { refPrSyncBranch, refPrSyncTarget = "feature/x", "main" }},
		{"push-branch missing flag", runRefineryPrPushBranch, func() { refPrPushBranch = "" }},
		{"push-branch outside a workspace", runRefineryPrPushBranch, func() { refPrPushBranch = "feature/x" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.set()
			cmd := &cobra.Command{}
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)

			err := tc.run(cmd, nil)
			code, ok := IsSilentExit(err)
			if !ok || code != syncExitRefused {
				t.Fatalf("got err=%v, want silent exit %d", err, syncExitRefused)
			}
			if stderr.Len() == 0 {
				t.Fatal("want the failure reason on stderr")
			}
		})
	}
}
