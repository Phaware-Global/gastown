+++
name = "assistant-sweep"
description = "Trigger the Mayor's Assistant loose-ends sweep (stalled decisions, blocked external actors, unslung work) at the configured interval; the assistant mails the Mayor a SWEEP: summary"
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
   handles the exact subject, body, priority, and message type — it is
   intentionally parameter-free to keep this dog cheap and deterministic.

2. Record the run as an ephemeral `type:plugin-run` receipt (labels
   `plugin:assistant-sweep` and `result:success`) and close it
   immediately, per the dispatch instructions.

3. Exit. Do not wait for the assistant's response.

## Why script-style content with agent execution

The body is fixed; agent execution exists so the dog can handle transient
mail-send failures (retry once, escalate if still failing) without a
second-class script gate. If `./run.sh` exits non-zero, retry once after a
short delay; if still failing, `gt escalate "assistant-sweep: mail
delivery failed" -s LOW` and record a `result:failure` wisp instead.

## What the trigger asks the assistant to do

The mail is a wake-up, not a procedure: it tells the Mayor's Assistant to
run `gt assistant sweep` (the deterministic port runner — beads by default,
Jira and GitHub when `assistant.sweep` names them), compose one `SWEEP:`
mail for the Mayor from the report (new findings in full, standing ones as
counts, ports that failed as "not checked", external text only inside the
report's fenced blocks, suggestions only from the report's Suggest lines),
send it, and then `gt assistant sweep --ack <run-id>` so the next sweep
dedupes against what the Mayor was told. If the command fails the assistant
mails "SWEEP: not checked" and stops; it never sweeps by hand.

The assistant reports and suggests; the Mayor decides. This dog does neither.
