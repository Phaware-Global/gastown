# PR Guard Does Not Respect merge_strategy=pr

## Problem

The `gt tap guard pr-workflow` (`internal/cmd/tap_guard.go`) blocks `gh pr create`
from any Gas Town agent context by checking for env vars like `GT_REFINERY`,
`GT_POLECAT`, etc. and CWD paths containing `/polecats/` or `/crew/`.

When a rig is configured with `merge_strategy=pr`, the refinery needs to create
PRs as part of its normal workflow. The guard blocks this unconditionally.

The guard was written for the default direct-to-main model and was never updated
when `merge_strategy=pr` was added.

## Evidence

- `internal/cmd/tap_guard.go:70-86` — hard-blocks in any agent context
- `internal/refinery/manager.go:220` — sets `GT_REFINERY=1` in refinery env
- `internal/refinery/engineer.go:710` — `doMergePR()` expects PR to already exist
  (calls `FindPRNumber`, does not create)
- `internal/formula/formulas/mol-refinery-patrol.formula.toml:637` — formula step
  instructs agent to run `gh pr create`, which the guard blocks

## Workaround

Hooks override at `~/.gt/hooks-overrides/jira_claude_channel__refinery.json` removes
the `Bash(gh pr create*)` guard for the specific rig's refinery. Requires
`gt hooks sync` after changes. Only affects one rig — other rigs retain the guard.

## Fork Fix

Modify `isGasTownAgentContext()` or `runTapGuardPRWorkflow()` to check the rig's
`merge_strategy` setting. Allow `gh pr create` from the refinery when
`merge_strategy=pr`. Roughly:

```go
func runTapGuardPRWorkflow(cmd *cobra.Command, args []string) error {
    if isGasTownAgentContext() {
        // Check if current rig uses PR merge strategy
        if rigAllowsPR() {
            return nil // allow
        }
        // ... existing block logic
    }
}
```
