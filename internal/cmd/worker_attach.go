package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/workerattach"
)

// gt worker-attach — the pane launcher for a remote polecat
// (internal/workerattach). It lives in gt, not only in the standalone
// gt-worker-attach binary, so that the one executable already allowed to reach
// the worker is the one the pane runs.
var workerAttachCmd = &cobra.Command{
	Use:                "worker-attach --address <addr> --session <id> -- <agent argv...>",
	Short:              "Attach this terminal to a remote polecat session (run by the pane)",
	Hidden:             true,
	DisableFlagParsing: true, // the agent argv after -- is passed through untouched
	Run: func(_ *cobra.Command, args []string) {
		os.Exit(workerattach.Main(args))
	},
}

func init() {
	rootCmd.AddCommand(workerAttachCmd)
}
