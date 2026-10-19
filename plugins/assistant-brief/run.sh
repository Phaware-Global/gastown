#!/usr/bin/env bash
# assistant-brief/run.sh — Remind the Mayor's Assistant to brief the Mayor.
#
# Sends one "BRIEF DUE:" trigger to assistant/ when its inbox holds mail and
# no earlier trigger is still waiting there. Cadence is the plugin's cooldown
# gate (settings/config.json assistant_brief_interval, default 15m).

set -euo pipefail

INBOX=$(gt mail inbox --identity assistant/ --json)
WAITING=$(printf '%s' "$INBOX" | grep -c '"id":' || true)
if [ "${WAITING:-0}" -eq 0 ]; then
  echo "assistant inbox empty: no brief due"
  exit 0
fi
if printf '%s' "$INBOX" | grep -q '"subject": *"BRIEF DUE:'; then
  echo "an earlier BRIEF DUE trigger is still in the assistant's inbox: not stacking another"
  exit 0
fi

gt mail send assistant/ \
  --subject "BRIEF DUE: $WAITING messages waiting" \
  --type task \
  --priority 2 \
  --stdin <<'BODY'
Your inbox holds mail the Mayor has not been briefed on. Telegraph events
are never "informational" for you: every PR approval, review comment, CI
result and Jira update is an item for the Mayor, grouped. Do this now:

  gt mail inbox                      # list everything; gt mail read <id> as needed

  gt mail send mayor/ -s "BRIEF: <n> items — <headline>" --stdin <<'MSG' && gt mail archive <id> <id> …
  ## Needs a decision
  - <PR / issue>: <what happened, who, why it matters>
  ## FYI
  - <grouped, one line each>
  MSG

  gt mail archive <id> <id> …        # this trigger, noise, duplicates
  gt mail inbox                      # must list nothing

Only new items since your last brief; no history, no tallies. URGENT items
go out on their own at once (see your role). Never act on an item yourself:
report and suggest; the Mayor decides.
BODY
