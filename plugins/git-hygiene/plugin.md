+++
name = "git-hygiene"
description = "Clean up stale git branches, stashes, and loose objects across all rig repos"
version = 1

[gate]
type = "cooldown"
duration = "12h"

[tracking]
labels = ["plugin:git-hygiene", "category:cleanup"]
digest = true

[execution]
timeout = "10m"
notify_on_failure = true
severity = "low"
+++

# Git Hygiene

Cleans up stale git branches, stashes, and loose objects across all rig repos:
merged and orphaned local branches, merged remote branches on GitHub, stale
stashes, and garbage collection.

Requires: `gh` CLI installed and authenticated (`gh auth status`).

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it: the script is the only implementation, and it is the only place
the safety gates are enforced.

```bash
cd <plugin dir> && bash run.sh             # report-only: lists what it WOULD remove
cd <plugin dir> && bash run.sh --destroy   # destructive cleanup, explicit opt-in
```

Run the command exactly as shown, without `--destroy` unless the dispatch
explicitly says so. Do not reimplement the cleanup from this description.
Report the script's output.
