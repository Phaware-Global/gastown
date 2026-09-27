+++
name = "submodule-commit"
description = "Auto-commit accumulated changes inside git submodules and update parent pointer"
version = 1

[gate]
type = "cooldown"
duration = "2h"

[tracking]
labels = ["plugin:submodule-commit", "category:git-hygiene"]
digest = true

[execution]
timeout = "15m"
notify_on_failure = true
severity = "low"

# Opt-in per rig via plugin frontmatter:
# [plugin.submodule-commit]
# enabled = true
# commit_branch = "main"          # branch to commit on in each submodule
# push_enabled = false            # push submodule commits (false = local only)
# allowlist = []                  # empty = all submodules; ["path/to/sub"] = only those
+++

# Submodule Commit

Auto-commits accumulated changes inside git submodules and updates the parent
repo's submodule pointer. Polecats only operate on parent repo worktrees and
have no commit mandate for submodule repos — this plugin fills that gap.

**Opt-in only.** A rig must enable it in its rig config
(`plugins.submodule-commit.enabled=true`); `run.sh` skips every other rig.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it.

```bash
cd <plugin dir> && bash run.sh
```

Run the command exactly as shown. Do not reimplement the commit flow from this
description. Report the script's output.
