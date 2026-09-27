+++
name = "rebuild-gt"
description = "Rebuild stale gt binary from gastown source"
version = 2

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

**SAFETY**: This plugin MUST only rebuild forward (binary ancestor of HEAD) and
only from the main branch. Rebuilding to an older or diverged commit caused a
crash loop where every new session's startup hook failed, the witness respawned
it, and the loop repeated every 1-2 minutes. `run.sh` enforces this: it skips
the rebuild when the repo is on a non-main branch, is dirty, or HEAD is not a
descendant of the binary's commit.

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
