+++
name = "dolt-snapshots"
description = "Tag Dolt databases at convoy boundaries for audit, diff, and rollback"
version = 3

[gate]
type = "event"
on = "convoy.created"

[tracking]
labels = ["plugin:dolt-snapshots", "category:data-safety"]
digest = true

[execution]
timeout = "2m"
notify_on_failure = true
severity = "low"
+++

# Dolt Snapshots v3

Snapshots Dolt databases at convoy lifecycle boundaries using **tags** (immutable)
and optionally **branches** (mutable, for working diffs).
Implemented as a standalone Go binary with parameterized SQL — no shell
interpolation, no subshell bugs, no auto-committing dirty state.

## Run

`run.sh` builds the binary if needed and execs it, passing its arguments
through. It is the only entry point; this file carries no executable copy of it.

```bash
cd <plugin dir> && bash run.sh --cleanup     # one-shot snapshot cycle
cd <plugin dir> && bash run.sh --watch       # tail the events log, snapshot on convoy events
cd <plugin dir> && bash run.sh --dry-run     # show what would be done, change nothing
```

Run the command as dispatched. Do not reimplement the snapshot logic from this
description. Report the script's output.

## What this enables

- **Convoy audit** — verify agents did what they were supposed to, by diffing a
  snapshot tag against `HEAD` with `dolt_diff` / `dolt_diff_stat`.
- **Convoy rollback** — revert a database, or a single table, to its pre-convoy
  state by checking out the snapshot tag.
- **Cross-convoy comparison** — `dolt_diff` between two snapshot tags tracks
  progress between runs.
- **Data loss investigation** — when backup alerts fire, diff against the last
  snapshot and filter on `diff_type = 'removed'`.

## What branches enable (mutable sandboxes)

Branches are writable copies of the database at snapshot time. Unlike tags,
you can commit to them — making them useful for:

- **Dry-run convoy work** — test bulk operations without touching main
- **Isolated convoy writes** — agents write to branch, refinery merges
- **What-if analysis** — test theories without risk
- **Parallel convoy isolation** — two convoys write to separate branches

## Why tags over branches

- A branch is just a pointer that moves with new commits — not a true snapshot
- A tag is immutable: it always points to the exact state when the convoy was
  staged
- Tags survive branch cleanup and are cheaper to keep long-term
- `dolt diff` between two tags works

## Trigger

This is one of three event-gated plugins sharing the same Go binary:

| Plugin | Event | Snapshot |
|--------|-------|----------|
| `dolt-snapshots` | `convoy.created` | `open/` tags (pre-work baseline) |
| `dolt-snapshots-staged` | `convoy.staged` | `staged/` tags + branches (staging baseline) |
| `dolt-snapshots-launched` | `convoy.launched` | `staged/` tags + branches (launch baseline) |

Each fires on its specific event. The binary is idempotent — it checks all
convoys and creates whichever tags/branches are missing.

The binary connects using gastown's standard Dolt config and reads
`routes.jsonl` to discover rig databases. In `--watch` mode it tails the events
log and runs a snapshot cycle immediately (<1s) when convoy events are detected,
which matters for `convoy.launched` where agents start writing to databases
immediately.
