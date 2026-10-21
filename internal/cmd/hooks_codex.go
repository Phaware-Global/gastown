package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/hooks"
)

var hooksCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Managed hooks for Codex agents",
	Long: `Manage the hooks Codex agents run.

Codex runs Gas Town's hooks as managed hooks from its system requirements
file (` + hooks.CodexRequirementsPath + `). That file is root-owned, so an
agent cannot edit or untrust the hooks that guard it. Each hook calls
'gt hooks codex run <event>', which runs the same hooks a Claude agent in
that role gets (base config plus role and rig overrides).

Install or update the requirements file (needs sudo):
  gt hooks codex requirements | sudo tee ` + hooks.CodexRequirementsPath + `

Gas Town refuses to start a Codex agent until 'gt hooks codex check' passes.
The file names the gt binary by path, so reinstall it only when that path
changes; guard changes arrive with gt updates.`,
	RunE: requireSubcommand,
}

var hooksCodexRequirementsCmd = &cobra.Command{
	Use:   "requirements",
	Short: "Print the Codex requirements.toml that installs the managed hooks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Print(hooks.CodexRequirementsTOML(hooks.GTBinary()))
		return nil
	},
}

var hooksCodexCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Verify the Codex managed hooks are installed for this gt binary",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := hooks.VerifyCodexManagedHooks(); err != nil {
			return err
		}
		fmt.Printf("Codex managed hooks are installed in %s\n", hooks.CodexRequirementsPath)
		return nil
	},
}

var hooksCodexRunCmd = &cobra.Command{
	Use:    "run <event>",
	Short:  "Run the Gas Town hooks for a Codex hook event (called by Codex)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	// A block is an exit-2 SilentExit; usage text would land in the
	// reason Codex shows the model.
	SilenceUsage: true,
	RunE:         runHooksCodexRun,
}

func init() {
	hooksCmd.AddCommand(hooksCodexCmd)
	hooksCodexCmd.AddCommand(hooksCodexRequirementsCmd, hooksCodexCheckCmd, hooksCodexRunCmd)
}

// runHooksCodexRun is the managed hook. It runs for every Codex session on
// the machine, so a session outside Gas Town gets no hooks at all.
func runHooksCodexRun(cmd *cobra.Command, args []string) error {
	if !isGasTownAgentContext() {
		return nil
	}
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading hook payload: %w", err)
	}

	role := os.Getenv("GT_ROLE")
	if role == "" {
		fmt.Fprintln(os.Stderr, "gt hooks codex: GT_ROLE is not set; running the base hooks only")
	}
	cfg, err := hooks.ComputeExpected(hooks.TargetForRole(role))
	if err != nil {
		// A broken override must not switch the guards off.
		fmt.Fprintf(os.Stderr, "gt hooks codex: %v; running the default base hooks\n", err)
		cfg = hooks.DefaultBase()
	}

	res := hooks.DispatchCodex(args[0], payload, cfg, hooks.RunCodexHookCommand)
	_, _ = os.Stdout.Write(res.Stdout)
	_, _ = os.Stderr.Write(res.Stderr)
	if res.ExitCode != 0 {
		return NewSilentExit(res.ExitCode)
	}
	return nil
}
