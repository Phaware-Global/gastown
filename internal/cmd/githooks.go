package cmd

import (
	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/git"
)

// disableHooksForPRCheckoutCommands turns git hooks off for the commands that
// run git over pull request content. Their agent sessions already carry the
// same setting; this covers an invocation from anywhere else.
//
// Commands that start sessions stay out: a tmux server started by one of them
// would hand its environment to every later session, polecats included.
func disableHooksForPRCheckoutCommands(cmd *cobra.Command) error {
	for c := cmd; c != nil; c = c.Parent() {
		switch c {
		case reviewerCheckoutCmd, refineryPrCmd, mqIntegrationLandCmd:
			return git.DisableHooksForProcess()
		}
	}
	return nil
}
