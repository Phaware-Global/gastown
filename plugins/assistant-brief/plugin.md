+++
name = "assistant-brief"
description = "Remind the Mayor's Assistant to brief the Mayor when its inbox holds mail: one trigger carrying the exact brief-and-archive commands"
version = 1

[gate]
type = "cooldown"
# Effective cooldown: settings/config.json `assistant_brief_interval` when it
# is set, parseable and at least 5m; otherwise this static duration.
duration = "15m"
duration_setting = "assistant_brief_interval"

[tracking]
labels = ["plugin:assistant-brief", "category:coordination"]
digest = true

[execution]
timeout = "2m"
notify_on_failure = true
severity = "low"
+++

# Mayor's Assistant Brief Trigger

You are a dog dispatched by the Deacon at the town's brief cadence
(`assistant_brief_interval`, default 15m). Your only job is to send the
Mayor's Assistant one trigger mail when its inbox holds mail, so that
briefing the Mayor happens on schedule instead of whenever the assistant
decides to. You do not read, brief or archive anything yourself.

Why it exists: on the live town the assistant (nemotron-3-ultra) left 73
telegraph events unbriefed for six hours, calling them "informational",
yet briefed and archived all of them within three minutes of a mail that
carried the exact commands. The sweep has the same shape and runs reliably
because its trigger carries the commands; this plugin gives briefing that
trigger.

## Deploying to a town

The daemon scans `<town>/plugins`, not this repository, and the plugin sync
is additive. Copy `plugins/assistant-brief` from the repository into
`<town>/plugins/`.

## Steps

1. Run `./run.sh`. It sends nothing when the assistant's inbox is empty or a
   previous trigger is still unarchived there; otherwise it sends one
   `BRIEF DUE:` mail to `assistant/`.
2. Record the run as an ephemeral `type:plugin-run` receipt (labels
   `plugin:assistant-brief` and `result:success`) and close it immediately,
   per the dispatch instructions.
3. Exit. Do not wait for the assistant.

If `./run.sh` exits non-zero, retry once after a short delay; if it still
fails, `gt escalate "assistant-brief: trigger failed" -s LOW` and record a
`result:failure` wisp instead.
