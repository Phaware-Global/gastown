package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/githooks"
	"github.com/steveyegge/gastown/internal/workspace"
)

// disableHooksForPRCheckoutCommands keeps a checkout's hooks from running in
// the commands that run git over pull request content. Their agent sessions
// already carry the same setting; this covers an invocation from anywhere else.
//
// Commands are listed one by one. One that starts sessions must stay out: a
// tmux server started by it would hand its environment to every later session,
// polecats included.
func disableHooksForPRCheckoutCommands(cmd *cobra.Command) error {
	switch cmd {
	case reviewerCheckoutCmd:
		return git.SetHooksPathForProcess(constants.NoHooksPath)
	case refineryPrSyncBranchCmd, refineryPrPushBranchCmd, mqIntegrationLandCmd:
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return fmt.Errorf("locating trusted git hooks: %w", err)
		}
		dir, err := githooks.Install(townRoot)
		if err != nil {
			return err
		}
		return git.SetHooksPathForProcess(dir)
	}
	return nil
}
