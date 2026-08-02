package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/socket"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt worker push-binaries — refresh a worker's gastown companions on demand
// (docs/design/remote-polecat-execution-socket.md §4.1).
//
// Provision does this automatically when versions differ; this is the operator
// handle for the cases automation cannot cover: pushing before a session is
// needed, re-pushing after a failure, or seeing WHY a worker is being skipped —
// the automatic path logs and steps over failures so a version bump never fails
// a polecat start, which is exactly what makes an explicit command necessary.
var workerPushBinariesCmd = &cobra.Command{
	Use:   "push-binaries <rig>",
	Short: "Push this orchestrator's gastown binaries to a rig's worker",
	Long: `Push gt-proxy-client and gt-worker-client to the worker configured for a rig.

The worker's gt-proxy-client IS the agent's gt and bd, and both binaries are
version-coupled to this orchestrator's control plane, so a drifted worker fails
in ways that look like agent bugs.

The worker installs gt-proxy-client immediately. gt-worker-client is the running
service, so it is staged and applied when that worker next has no live session —
a restart would abandon a polecat mid-work.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rigName := args[0]
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		rigPath := filepath.Join(townRoot, rigName)

		settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
		if err != nil {
			return fmt.Errorf("loading settings for rig %s: %w", rigName, err)
		}
		execCfg := settings.Execution
		if execCfg == nil || execCfg.BackendName() != socket.BackendName {
			return fmt.Errorf("rig %s does not use the socket execution backend (nothing to push to)", rigName)
		}

		b, err := socket.New(execCfg)
		if err != nil {
			return err
		}
		results, err := b.PushBinariesTo(context.Background())
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("Worker is already on this orchestrator's version; nothing to push.")
			return nil
		}
		for _, r := range results {
			fmt.Printf("  %-18s %s\n", r.Name, r.Applied)
		}
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}
		return nil
	},
}

func init() {
	workerPushBinariesCmd.Flags().Bool("json", false, "emit the per-binary result as JSON")
	workerCmd.AddCommand(workerPushBinariesCmd)
}
