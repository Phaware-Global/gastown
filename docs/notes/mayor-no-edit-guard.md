# Mayor Has No Guard Against Direct Code Implementation

## Problem

The mayor prompt template (`internal/templates/roles/mayor.md.tmpl`) clearly
states the mayor is a "coordinator, not a solo coder" with a "File It, Sling It"
work philosophy. However, no PreToolUse hook enforces this. The mayor runs with
`--dangerously-skip-permissions` and can freely use `Edit`, `Write`, and any
other tool.

In practice, when given detailed implementation requests, the mayor attempts to
implement the code directly rather than filing beads and slinging to polecats.

## Workaround

Created hooks override at `~/.gt/hooks-overrides/mayor.json` that blocks `Edit`
and `Write` tool calls with a message directing the mayor to `gt prime` and
sling work. Propagated via `gt hooks sync`.

## Fork Fix

Add default `Edit` and `Write` guards to the mayor's hook template at
`internal/hooks/templates/claude/settings-autonomous.json` (or the equivalent
base hooks config), conditioned on the mayor role. The guard should be built-in
rather than requiring manual override configuration.

This should also be reflected in the template — the "File It, Sling It" section
should note that Edit/Write are hard-blocked, not just discouraged.
