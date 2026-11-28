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

The command starts without the variables that lead to the town's tmux server,
town directory or Dolt server, with a private TMUX_TMPDIR that is removed
afterwards, and with the Dolt port set to one nothing listens on. Everything
else, including the working directory, is inherited.

For a shell line, pass a shell: gt reviewer exec -- sh -c 'cd x && go test'

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
