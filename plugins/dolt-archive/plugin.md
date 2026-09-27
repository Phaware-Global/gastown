+++
name = "dolt-archive"
description = "Offsite backup: JSONL snapshots to git, dolt push to GitHub/DoltHub"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:dolt-archive", "category:data-safety"]
digest = true

[execution]
timeout = "15m"
notify_on_failure = true
severity = "critical"
+++

# Dolt Archive

Gets production data off this machine. Three layers:

1. **JSONL export** — human-readable, diffable snapshots of each production
   database. The last-resort recovery layer: always maintained, regardless of
   whether the other layers work.
2. **Git push** — the JSONL snapshots are committed to a backup repository and
   pushed.
3. **Dolt push** — native Dolt replication to configured remotes.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it: the script is the only implementation, and it is the only place
the remote-visibility guard is enforced.

```bash
cd <plugin dir> && bash run.sh
cd <plugin dir> && bash run.sh --databases db1,db2   # specific databases only
cd <plugin dir> && bash run.sh --skip-git            # skip the git layer
cd <plugin dir> && bash run.sh --skip-dolt-push      # skip the dolt push layer
```

Run the command exactly as shown. Do not reimplement any layer from this
description. Report the script's output.

## Visibility guard

See `visibility_guard.sh`. Before pushing any remote, `run.sh` resolves it to a
GitHub `owner/repo` and checks its visibility via `gh api`. A push proceeds
only when the repo is confirmed **private**. A public repo, a non-GitHub
remote, a missing `gh`, or a failed lookup all refuse to push — fail closed,
not fail open. A refusal is logged, counted separately from a normal push
failure, escalated with a fingerprint, and surfaced in the cycle summary as
`dolt_push_refused`.
