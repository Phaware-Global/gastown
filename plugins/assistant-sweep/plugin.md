+++
name = "assistant-sweep"
description = "Trigger the Mayor's Assistant loose-ends sweep at the configured interval; the trigger carries the procedure for assistant.sweep.mode"
version = 2

[gate]
type = "cooldown"
# Effective cooldown: settings/config.json `assistant_sweep_interval` when it
# is set, parseable and at least 5m; otherwise this static duration.
duration = "1h"
duration_setting = "assistant_sweep_interval"

[tracking]
labels = ["plugin:assistant-sweep", "category:coordination"]
digest = true

[execution]
timeout = "2m"
notify_on_failure = true
severity = "low"
+++

# Mayor's Assistant Sweep

You are a dog dispatched by the Deacon at the town's configured sweep interval
(`assistant_sweep_interval`, default 1h). Your only job is to send the Mayor's
Assistant one trigger mail so the periodic loose-ends sweep happens on
schedule. This replaces the old `mayor-loose-ends-nudge` plugin, which nudged
the Mayor directly; the Mayor now receives the *result* of the sweep as a
`SWEEP:` mail from the assistant instead of a reminder to go sweep.

You do **not** sweep anything yourself. You send one mail. The assistant does
the work and briefs the Mayor.

## Deploying to a town

The daemon scans `<town>/plugins`, not this repository, and the daemon's
plugin sync is additive. Deploy this plugin explicitly, per plugin: copy
`plugins/assistant-sweep` from the repository into `<town>/plugins/`, then
delete `<town>/plugins/mayor-loose-ends-nudge` (otherwise both plugins fire).
Do **not** run a whole-tree `gt plugin sync --clean`, which would also
overwrite deployed plugins that have drifted on purpose.

## Steps

1. Run `./run.sh` to send the trigger mail to `assistant/`. The script
   handles the exact subject, body (from `gt assistant sweep --protocol`),
   priority, and message type — it is intentionally parameter-free to keep
   this dog cheap and deterministic.

2. Record the run as an ephemeral `type:plugin-run` receipt (labels
   `plugin:assistant-sweep` and `result:success`) and close it
   immediately, per the dispatch instructions.

3. Exit. Do not wait for the assistant's response.

## Why script-style content with agent execution

The body comes from gt; agent execution exists so the dog can handle transient
mail-send failures (retry once, escalate if still failing) without a
second-class script gate. If `./run.sh` exits non-zero, retry once after a
short delay; if still failing, `gt escalate "assistant-sweep: mail
delivery failed" -s LOW` and record a `result:failure` wisp instead.

## What the trigger asks the assistant to do

The mail body is the procedure for the town's `assistant.sweep.mode`, as
printed by `gt assistant sweep --protocol` (a static deterministic fallback
is used when that command is unavailable):

- **deterministic** (default): run `gt assistant sweep --send-to mayor/`,
  which mails the Mayor the report verbatim when something is new or a port
  did not fully run, sends nothing otherwise, and acknowledges the run; then
  archive the trigger.
- **inference**: save the report, read it, verify findings read-only where
  useful, decide what the Mayor needs (the report with a header, a BRIEF
  that includes it, or nothing), then `gt assistant sweep --ack` with the
  run ID from the saved file. A port that did not fully run always reaches
  the Mayor.

In both modes, if the command fails the assistant mails "SWEEP: not
checked" and stops; it never sweeps by hand.

The assistant reports and suggests; the Mayor decides. This dog does neither.
