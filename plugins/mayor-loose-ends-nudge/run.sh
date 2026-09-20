#!/usr/bin/env bash
# mayor-loose-ends-nudge/run.sh — Send a focused 5h nudge to the Mayor.
#
# Body is fixed. The Mayor does the work; this script just delivers the kick.
# Idempotency is provided by the plugin's cooldown gate (5h), not this script.

set -euo pipefail

SUBJECT="Loose-ends sweep — 5h nudge"

gt mail send mayor/ \
  --subject "$SUBJECT" \
  --type task \
  --priority 2 \
  --stdin <<'BODY'
Sweep loose ends now. Act on each bucket; don't queue.

Do not ask for approval to proceed. Open questions or decisions go as
Jira comments on the relevant issue — then move immediately to the
next loose end. Don't park work waiting for a reply, and don't mail
the overseer asking what to do next.

1. Jira — unacknowledged mentions, unresponded comments. Use the Telegraph
   inbox and your normal Jira flow. Reply, escalate, or close the loop.
   DO NOT POST PATROL STATUS, ONLY RESPOND TO UNANSWERED USER COMMENTS OR
   POST WHERE USER INPUT IS REQUIRED TO UNBLOCK
2. Blocked PRs — for each PR stuck on internal reviewer or human input,
   check the linked Jira issue first. If no internal reviewer loop has been 
   performed, start one.  If no human-review request has been
   posted yet for the current changes (head SHA), post one. If a request
   already exists for this revision, leave it alone unless the wait has 
   been excessive — only re-ping if more than a day has past since 
   review was requested.
3. Stalled beads — open work with no recent progress. Reassign, sling
   onward, or close with reason.
4. Unslung work — beads ready to dispatch but unhooked. Sling them.
5. Assigned work in Research/TODO/In-Progress - Find all Jira tasks assigned to you (Artie) in
   the Research, TODO, or In Progress statuses and either begin/continue work or add a comment about why work
   is blocked (if such a comment does not already exist)

Skip a bucket only if it's actually empty. If everything is clear, reply
"all clear" to this thread so the trail is visible.
BODY
