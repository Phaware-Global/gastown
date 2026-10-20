#!/usr/bin/env bash
# assistant-sweep/run.sh — Trigger the Mayor's Assistant loose-ends sweep.
#
# Body is fixed. The assistant does the sweep and mails the Mayor a SWEEP:
# summary; this script just delivers the trigger. Cadence is the plugin's
# cooldown gate, sourced from settings/config.json assistant_sweep_interval
# (default 1h) — not this script.

set -euo pipefail

SUBJECT="SWEEP: loose-ends sweep due"

# The body is the procedure for the town's assistant.sweep.mode, printed by
# gt itself; the fallback is the deterministic procedure, so an older gt or a
# settings error still sends a trigger that works.
BODY=$(gt assistant sweep --protocol 2>/dev/null) || BODY=""
if [ -z "$BODY" ]; then
  BODY='Run the deterministic sweep; the command decides what the Mayor gets:

  gt assistant sweep --send-to mayor/

Then archive this trigger mail. If the command fails, mail the Mayor
"SWEEP: not checked: <error>" with the error text as the body and stop.'
fi

printf '%s\n' "$BODY" | gt mail send assistant/ \
  --subject "$SUBJECT" \
  --type task \
  --priority 2 \
  --stdin
