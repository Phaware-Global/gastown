#!/usr/bin/env bash
# assistant-sweep/run.sh — Trigger the Mayor's Assistant loose-ends sweep.
#
# Body is fixed. The assistant does the sweep and mails the Mayor a SWEEP:
# summary; this script just delivers the trigger. Cadence is the plugin's
# cooldown gate, sourced from settings/config.json assistant_sweep_interval
# (default 1h) — not this script.

set -euo pipefail

SUBJECT="SWEEP: loose-ends sweep due"

gt mail send assistant/ \
  --subject "$SUBJECT" \
  --type task \
  --priority 2 \
  --stdin <<'BODY'
Run the deterministic sweep; the command mails the Mayor the report itself
and acknowledges the run once the mail was sent:

  gt assistant sweep --send-to mayor/

Then archive this trigger mail. That is the whole job. Do not run the
sweep any other way, do not retype or summarize the report, and never
sweep Jira, GitHub, Telegraph mail or beads by hand instead.

If the command fails, mail the Mayor "SWEEP: not checked: <error>" with the
error text as the body and stop; the run stays pending and the next sweep
reports everything again.
BODY
