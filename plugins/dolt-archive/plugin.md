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
3. **Dolt replication** — native Dolt to configured remotes, run by `run.sh`.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it: the script is the only implementation, and it is the only place
the remote-visibility guard (both push layers) is enforced.

```bash
cd <plugin dir> && bash run.sh
cd <plugin dir> && bash run.sh --databases db1,db2   # specific databases only
cd <plugin dir> && bash run.sh --skip-git            # skip the git layer
cd <plugin dir> && bash run.sh --skip-dolt-push      # skip the Dolt replication layer
```

Run the command exactly as shown. Do not reimplement any layer from this
description. Report the script's output.

## Visibility guard

See `visibility_guard.sh`. Both push layers — the JSONL git push (layer 2) and
the native Dolt push (layer 3) — clear the same fail-closed check before
anything leaves the machine: `run.sh` resolves the destination to a GitHub
`owner/repo` and checks its visibility via `gh api`. A push proceeds only when
the repo is confirmed **private**. A public repo, a non-GitHub remote
(including DoltHub), a missing `gh`, or a failed lookup all refuse to push —
fail closed, not fail open. For the git layer the destination judged is what
`git push origin` would actually use (`git remote get-url --push --all
origin`, so `pushurl`/`insteadOf` rewrites count, and every push URL must
pass). A refusal is logged, counted separately from a normal push failure,
escalated with a fingerprint, and surfaced in the cycle summary as
`dolt_push_refused` (Dolt layer) or `git=refused` / `git_push_refused` (git
layer).

## Escalation dedupe

`run.sh` escalates each unhealthy condition **once**, not once per run
(`gt escalate --fingerprint` alone isn't enough: it only suppresses against
*open* beads, so a closed repeat would otherwise be re-filed next cycle).
Every *independently-resolvable* condition — a specific db, or a specific
db+remote pair, never a whole condition class lumped into one shared set —
is tracked in its own state file under `~/gt/.dolt-archive/escalation-state/`
(override: `DOLT_ARCHIVE_STATE_DIR`), with no history shared between them: one
entity's ongoing failure can never mask another entity's resolve-then-return.
A condition unchanged since its last escalation stays quiet (one `low`
"still unresolved" digest after `ESCALATION_REPEAT_SECS`, default 86400); new,
changed, or returned-after-resolving escalates immediately at `critical`; a
condition a run checked and found clean drops its state, so a later
recurrence escalates afresh; a condition a run didn't check (a `--databases`
subset, or a layer that didn't run) is left untouched either way.
