+++
name = "feedback-dialog-watcher"
description = "Auto-dismiss Claude Code feedback/rating dialogs that intercept input and stall agents"
version = 1

[gate]
type = "cooldown"
duration = "5m"

[tracking]
labels = ["plugin:feedback-dialog-watcher", "category:health"]
digest = true

[execution]
timeout = "1m"
notify_on_failure = true
severity = "medium"
+++

# Feedback Dialog Watcher

Claude Code occasionally displays a feedback/rating dialog
(`1: Bad  2: Fine  3: Good  0: Dismiss`) that intercepts the input box until
a key is sent. Any agent whose nudges arrive while the dialog is open never
gets to process them — the typed text sits in the box and the agent appears
stalled. The Mayor traced a recurring class of agent stalls
(reference: hq-2dr55, gastown#81) to this dialog and confirmed that
`tmux send-keys -t <session> '0'` (the Dismiss option) clears it and
unblocks the agent immediately.

This plugin scans every active tmux pane every 5 minutes, looks for the
dialog's distinctive multi-marker text, and sends `'0'` to dismiss it. It
takes no other action and never sends a *rating* keystroke (`1`/`2`/`3`) —
only Dismiss, which is invariably the right answer for an automated agent
that didn't open the dialog deliberately.

## Why scan every pane (no allow-list)

`stuck-agent-dog` is scoped narrowly because it can *kill* sessions, and
killing the wrong session destroys human work. This plugin's only action is
to send the **Dismiss** key to a dialog that is already blocking input —
the worst case for a false positive is a stray `0` keystroke into a pane
whose content happened to match the multi-marker regex, which is harmless
compared to the systemic stall the dialog causes. Specificity comes from
the *detection*, not from an allow-list of sessions.

## Detection pattern

Match requires BOTH of these strings present in the captured pane content
(the dialog renders them together; either alone is too generic):

- `1: Bad`
- `0: Dismiss`

If either is missing, no match. Capture only the bottom ~30 lines (the
visible screen plus a small cushion) so historical scrollback that
incidentally contains the strings cannot trigger a stale dismiss.

## Step 1: Enumerate every tmux pane

```bash
# Bail early if no tmux server is running — nothing to do.
if ! tmux list-sessions >/dev/null 2>&1; then
  echo "No tmux server running; nothing to scan."
  exit 0
fi

# `-a` walks every pane across every session in one call. `pane_id` is a
# stable, unambiguous target (e.g. %12) — safer than session:window.pane
# when window/pane indices renumber under user activity mid-scan.
PANE_IDS=$(tmux list-panes -a -F '#{pane_id}')
```

## Step 2: Detect + dismiss

```bash
DISMISSED=0
SCANNED=0

while IFS= read -r PANE; do
  [ -z "$PANE" ] && continue
  SCANNED=$((SCANNED + 1))

  CONTENT=$(tmux capture-pane -t "$PANE" -p -S -30 2>/dev/null || true)
  [ -z "$CONTENT" ] && continue

  # Both markers must be present. Either alone is too generic.
  if grep -qF '1: Bad' <<<"$CONTENT" && grep -qF '0: Dismiss' <<<"$CONTENT"; then
    tmux send-keys -t "$PANE" '0'
    DISMISSED=$((DISMISSED + 1))
  fi
done <<< "$PANE_IDS"
```

## Step 3: Record receipt

```bash
SUMMARY="feedback-dialog-watcher: scanned $SCANNED panes, dismissed $DISMISSED dialog(s)"
echo "$SUMMARY"

bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:feedback-dialog-watcher,result:success \
  -d "$SUMMARY" --silent 2>/dev/null || true
```

The receipt bead is `--ephemeral` so it auto-prunes — successful runs are
high-volume signal that nothing-was-wrong, and accumulating one permanent
bead per cycle would pollute Dolt history. The deacon digest still picks
up the labels for plugin-run rollup.

## Failure behavior

Failures escalate via the standard plugin contract (`notify_on_failure =
true`, `severity = "medium"`). `set -euo pipefail` in run.sh causes any
unexpected exit to bubble up; tmux operations on individual panes are
wrapped in `|| true` so one weird pane doesn't fail the whole sweep.
