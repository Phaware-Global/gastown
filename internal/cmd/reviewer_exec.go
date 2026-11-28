package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/reviewer"
)

var reviewerExecCmd = &cobra.Command{
	Use:   "exec -- <command> [args...]",
	Short: "Run PR code away from the town's tmux and Dolt servers",
	Long: `Run a command that executes code from the pull request under review, such
as its tests or build scripts.

The command starts in a private temporary directory, not in the checkout, so
name the checkout: it is in GT_PR_CHECKOUT, and "$PWD" in your own shell.

  gt reviewer exec -- go -C "$PWD" test ./...
  gt reviewer exec -- make -C "$PWD" test
  gt reviewer exec -- env -C "$PWD/web" npm test

It also starts without the variables that lead to the town's tmux server, town
directory or beads store, with a private TMUX_TMPDIR, and with the Dolt port of
gt and bd forced to one nothing listens on. Everything else is inherited.

This guards against accidents, not hostile code: the command can still read and
write files, including the town's, and can reach the town's servers by name.

The exit code is the command's.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		code, err := reviewer.RunPRCommand(args, os.Stdin, os.Stdout, os.Stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			// The command's own output is the report; add nothing to it.
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			return NewSilentExit(code)
		}
		return nil
	},
}

func init() {
	// The command's own flags are not ours, with or without "--".
	reviewerExecCmd.Flags().SetInterspersed(false)
	reviewerCmd.AddCommand(reviewerExecCmd)
}
