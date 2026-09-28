+++
name = "compactor-dog"
description = "Monitor Dolt commit growth across production DBs and escalate when compaction is needed"
version = 1

[gate]
type = "cooldown"
duration = "30m"

[tracking]
labels = ["plugin:compactor-dog", "category:maintenance"]
digest = true

[execution]
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Compactor Dog

Monitors Dolt commit growth across all production databases and escalates to
the Mayor when history compaction or flatten is needed. This is a judgment
call, not a hard threshold trigger.

## Run

All logic lives in `run.sh`. This file intentionally carries no executable
copy of it. Without `--compact` the script only reports (check-only); compaction
is destructive (it flattens history and force-pushes) and requires an explicit
`--compact`, which a dispatch must not add on its own.

```bash
cd <plugin dir> && bash run.sh                  # report only
cd <plugin dir> && bash run.sh --check-only     # explicit report only
cd <plugin dir> && bash run.sh --dry-run        # report, do not compact
```

Run the command exactly as shown. Do not reimplement discovery, counting or
compaction from this description. Report the script's output.

## Judgment guidance

**You are a dog agent (Claude). Use the script's report, then use your judgment
to decide if maintenance is needed.** Consider:

- Commit count per DB (absolute size)
- Growth rate (commits per hour since last check)
- Time since last flatten or compaction
- Current swarm activity (more polecats = faster growth)
- Whether growth is "normal busy" or "runaway"

Guidelines (not rules — context matters):

| Signal | Comfortable | Getting warm | Escalate |
|--------|------------|--------------|----------|
| Total commits (per DB) | <200 | 200-500 | >500 |
| Hourly growth rate | <10/hr | 10-30/hr | >30/hr |
| Daily growth rate | <100/day | 100-300/day | >300/day |
| Time since flatten | <2 weeks | 2-4 weeks | >4 weeks |

But override the table if context warrants it:

- 400 commits after a 10-polecat swarm = normal, will settle
- 200 commits growing at 50/hr with no swarm = something's wrong
- Any DB over 1000 commits = escalate regardless

If you judge maintenance is needed, escalate to the Mayor with the per-database
numbers; do not compact yourself. If everything looks comfortable, just record
the result.
