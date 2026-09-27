+++
name = "mayor-loose-ends-nudge"
description = "Nudge the Mayor every 5h to sweep loose ends: Jira threads, blocked PRs, stalled beads, unslung work"
version = 1

[gate]
type = "cooldown"
duration = "5h"

[tracking]
labels = ["plugin:mayor-loose-ends-nudge", "category:coordination"]
digest = true

[execution]
timeout = "2m"
notify_on_failure = true
severity = "low"
+++

# Mayor Loose-Ends Nudge

You are a dog dispatched by the Deacon every 5 hours. Your only job is to send
the Mayor one focused, actionable nudge so loose ends do not accumulate
silently between attention cycles.

You do **not** investigate or act on these items yourself. You compose and
send one mail. The Mayor does the work.

## Steps

1. Run `./run.sh` to send the nudge mail to `mayor/`. The script handles the
   exact subject, body, priority, and message type — it is intentionally
   parameter-free to keep this dog cheap and deterministic.

2. Record the run as a wisp:

   ```bash
   bd create "mayor-loose-ends-nudge: dispatched" -t chore --ephemeral \
     -l type:plugin-run,plugin:mayor-loose-ends-nudge,result:success \
     -d "5h cooldown nudge dispatched to mayor/" \
     --silent 2>/dev/null || true
   ```

3. Exit. Do not wait for the Mayor's response.

## Why script-style content with agent execution

The body is fixed; agent execution exists so the dog can handle transient
mail-send failures (retry once, escalate if still failing) without a
second-class script gate. If `./run.sh` exits non-zero, retry once after a
short delay; if still failing, `gt escalate "mayor-loose-ends-nudge: mail
delivery failed" -s LOW` and record a `result:failure` wisp instead.

## What the nudge tells the Mayor to do

The mail body asks the Mayor to sweep four buckets:

1. **Unacknowledged Jira threads** — mentions and comments awaiting our reply.
   Use the Telegraph inbox + normal Jira flow. Reply or escalate.
2. **Blocked PRs** — when a PR is stuck on external review or human input,
   leave a comment on the linked Jira issue requesting help.
3. **Stalled beads** — open work without recent progress. Reassign, sling
   onward, or close with reason.
4. **Unslung work** — beads ready to dispatch but not yet on a hook. Sling
   them.

The Mayor decides priority and order. This dog does not.
