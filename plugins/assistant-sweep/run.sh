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
Run your periodic loose-ends sweep now and mail the Mayor ONE consolidated
"SWEEP:" summary. Report and suggest; do not act on items yourself.

Sweep these buckets (skip a bucket only if it is actually empty):

1. Stalled decisions — beads, escalations, or mail threads waiting on the
   Mayor. List each with its ID and what decision is needed.
2. Blocked external actors — PRs stuck on human review, unanswered Jira
   mentions or comments, review requests older than a day. Give the link
   and how long it has waited.
3. Stalled beads — open work with no recent progress (`bd list
   --status=open`, `bd blocked`, `gt convoy list`). Suggest reassign,
   sling onward, or close-with-reason.
4. Unslung work — `bd ready` items not on any hook. Suggest `gt sling`
   commands the Mayor can run verbatim.

Group related items. End with a short "Suggested actions" list. If every
bucket is clear, send "SWEEP: all clear" so the trail is visible.
BODY
