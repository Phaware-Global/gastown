+++
name = "gocache-trim"
description = "Keep the shared Go build cache under a size cap"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:gocache-trim", "category:maintenance"]
digest = true

[execution]
type = "script"
timeout = "10m"
notify_on_failure = true
severity = "medium"
+++

# Go Build Cache Trim

Go only drops build cache entries unused for 5 days. A host where many
worktrees build with different flags can fill its disk well before that.

When the cache (`go env GOCACHE`) is larger than the cap, `run.sh` drops the
entries unused for a day, then for 12, 6 and 2 hours, stopping as soon as it
fits. It never touches anything used in the last 2 hours, so it runs safely
alongside builds and tests. A cache still over the cap after that is reported
in the run output.

The cap is 10 GiB; set `GT_GOCACHE_CAP_MB` in the daemon's environment to
change it.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it.

```bash
cd <plugin dir> && bash run.sh
```
