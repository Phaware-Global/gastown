+++
name = "mayors-assistant-sweep"
description = "Trigger the Mayor's Assistant loose-ends sweep (stalled decisions, blocked external actors, unslung work) at the configured interval; the assistant mails the Mayor a SWEEP: summary"
version = 2

[gate]
type = "cooldown"
# Fallback only. The effective cooldown comes from settings/config.json
# `assistant_sweep_interval` (default "1h"); see docs/design/mayors-assistant-role.md.
duration = "1h"
duration_setting = "assistant_sweep_interval"

[tracking]
labels = ["plugin:mayors-assistant-sweep", "category:coordination"]
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

## Steps

1. Run `./run.sh` to send the trigger mail to `mayors-assistant/`. The script
   handles the exact subject, body, priority, and message type — it is
   intentionally parameter-free to keep this dog cheap and deterministic.

2. Record the run as an ephemeral `type:plugin-run` receipt (labels
   `plugin:mayors-assistant-sweep` and `result:success`) and close it
   immediately, per the dispatch instructions.

3. Exit. Do not wait for the assistant's response.

## Why script-style content with agent execution

The body is fixed; agent execution exists so the dog can handle transient
mail-send failures (retry once, escalate if still failing) without a
second-class script gate. If `./run.sh` exits non-zero, retry once after a
short delay; if still failing, `gt escalate "mayors-assistant-sweep: mail
delivery failed" -s LOW` and record a `result:failure` wisp instead.

## What the trigger asks the assistant to do

The mail body asks the Mayor's Assistant to sweep open work in beads and the
Telegraph trail, then mail the Mayor one `SWEEP:` summary covering:

1. **Stalled decisions** — beads or threads waiting on the Mayor.
2. **Blocked external actors** — PRs stuck on human review, unanswered Jira
   mentions, review requests older than a day.
3. **Stalled beads** — open work without recent progress.
4. **Unslung work** — beads ready to dispatch but not yet on a hook.

The assistant reports and suggests; the Mayor decides. This dog does neither.
