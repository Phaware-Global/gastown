+++
name = "gitignore-reconcile"
description = "Auto-untrack files that are tracked but match an active .gitignore rule"
version = 1

[gate]
type = "cooldown"
duration = "6h"

[tracking]
labels = ["plugin:gitignore-reconcile", "category:git-hygiene"]
digest = true

[execution]
timeout = "10m"
notify_on_failure = true
severity = "low"
+++

# Gitignore Reconcile

Scans all rig repos for files that are tracked in git but now match an active
`.gitignore` rule. On clean `main` branches, runs `git rm --cached` to untrack
them and commits. On dirty branches or active polecat worktrees, creates a
chore bead instead to avoid interference.

Root cause: `.gitignore` rules only block NEW files. Files committed before the
rule was added continue to be tracked until manually untracked.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it.

```bash
cd <plugin dir> && bash run.sh
```

Run the command exactly as shown. Do not reimplement the reconcile from this
description. Report the script's output.
