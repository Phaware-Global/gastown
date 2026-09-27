+++
name = "rebuild-gt"
description = "Rebuild stale gt binary from gastown source"
version = 3

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:rebuild-gt", "rig:gastown", "category:maintenance"]
digest = true

[execution]
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Rebuild gt Binary

Checks if the gt binary is stale (built from an older commit than HEAD) and
rebuilds it from source when that is safe.

Builds happen in a dedicated worktree (`$TOWN_ROOT/gastown/.gt-build`),
created with `git worktree add --detach` and reset to `origin/main` on every
run. The mayor's working clone (`$TOWN_ROOT/gastown/mayor/rig`) is never
built from and never modified — it routinely carries beads-state edits and a
local feature branch, and gating the rebuild on that clone being clean and on
main used to make the rebuild skip silently for hours (gt-f612).

**SAFETY**: This plugin MUST only rebuild forward (binary ancestor of HEAD).
Rebuilding to an older or diverged commit caused a crash loop where every new
session's startup hook failed, the witness respawned it, and the loop
repeated every 1-2 minutes. `run.sh` enforces this by refusing when
`origin/main` is not a forward descendant of the binary's commit, and
`make safe-install`'s `check-forward-only` target re-verifies it independently
at build time. If the binary is stale but the rebuild is skipped for any
reason (downgrade risk, missing source repo, worktree setup failure), `run.sh`
escalates after 3 consecutive skips instead of only writing an ephemeral
receipt.

Rebuilds use `make safe-install` (not `make install`) so the daemon is not
restarted while sessions are active; sessions pick up the new binary on their
next cycle.

The Deacon evaluates the cooldown gate before dispatch. If the gate is closed,
skip.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it.

```bash
cd <plugin dir> && bash run.sh
```

Run the command exactly as shown. Do not build the binary by hand from this
description. Report the script's output.
