#!/usr/bin/env bash
# feedback-dialog-watcher/run.sh — Auto-dismiss Claude Code feedback dialogs.
#
# Scans every tmux pane every cycle. When a pane shows the rating dialog
# ("1: Bad ... 0: Dismiss ..."), sends '0' to dismiss it so the agent's
# input box unblocks. The dialog otherwise traps drained nudges in the
# input box and the agent looks stalled (Mayor root cause: hq-2dr55,
# gastown#81).
#
# Action is non-destructive: only the Dismiss key is ever sent — never a
# rating (1/2/3). Specificity comes from the detection (both '1: Bad' AND
# '0: Dismiss' must appear in the bottom ~30 lines of the pane), not from
# a session allow-list.

set -euo pipefail

log() { echo "[feedback-dialog-watcher] $*"; }

# --- Preflight ---------------------------------------------------------------

if ! tmux list-sessions >/dev/null 2>&1; then
  log "No tmux server running; nothing to scan."
  _rid="$(bd create "feedback-dialog-watcher: no tmux server" \
    -t chore --ephemeral \
    -l type:plugin-run,plugin:feedback-dialog-watcher,result:success \
    --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
  exit 0
fi

# Stable pane targets (e.g. %12) survive concurrent user activity better
# than session:window.pane indices, which can renumber mid-scan.
PANE_IDS=$(tmux list-panes -a -F '#{pane_id}' 2>/dev/null || true)
if [[ -z "$PANE_IDS" ]]; then
  log "tmux server up but no panes returned; nothing to scan."
  _rid="$(bd create "feedback-dialog-watcher: 0 panes" \
    -t chore --ephemeral \
    -l type:plugin-run,plugin:feedback-dialog-watcher,result:success \
    --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
  exit 0
fi

# --- Scan + dismiss ----------------------------------------------------------

DISMISSED=0
SCANNED=0
DISMISSED_PANES=()

while IFS= read -r PANE; do
  [[ -z "$PANE" ]] && continue
  SCANNED=$((SCANNED + 1))

  # -S -30 captures the visible screen plus a small cushion. Larger windows
  # risk matching historical scrollback (e.g. a transcript that quoted the
  # dialog earlier) and re-sending Dismiss after the dialog is long gone.
  CONTENT=$(tmux capture-pane -t "$PANE" -p -S -30 2>/dev/null || true)
  [[ -z "$CONTENT" ]] && continue

  # Both markers required. Either alone is too generic:
  #   '1: Bad' alone matches any code/log line that lists '1: Bad' as data.
  #   '0: Dismiss' alone could match other Claude prompts that share the
  #   option label.
  if [[ "$CONTENT" == *"1: Bad"* ]] && [[ "$CONTENT" == *"0: Dismiss"* ]]; then
    # Tmux send-keys with a single literal key. No Enter needed — the
    # dialog responds to the keystroke immediately (Mayor verified).
    if tmux send-keys -t "$PANE" '0' 2>/dev/null; then
      DISMISSED=$((DISMISSED + 1))
      DISMISSED_PANES+=("$PANE")
      log "Dismissed dialog in pane $PANE"
    else
      log "WARN: send-keys to pane $PANE failed (pane may have closed mid-scan)"
    fi
  fi
done <<< "$PANE_IDS"

# --- Receipt -----------------------------------------------------------------

SUMMARY="feedback-dialog-watcher: scanned $SCANNED panes, dismissed $DISMISSED dialog(s)"
log "=== $SUMMARY ==="

# Detail body only when something happened — keep no-op receipts tiny.
if [[ $DISMISSED -gt 0 ]]; then
  DETAIL="$SUMMARY"$'\n\nDismissed panes:\n'"$(printf '  - %s\n' "${DISMISSED_PANES[@]}")"
else
  DETAIL="$SUMMARY"
fi

_rid="$(bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:feedback-dialog-watcher,result:success \
  -d "$DETAIL" --silent 2>/dev/null)" || true
[ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
