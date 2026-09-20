#!/usr/bin/env bash
# closed-bead-open-pr/run.sh — Detect review-fix beads closed while their
# linked PR is still open (possible false closure).
#
# DETECT ONLY. Never reopens beads, never comments on PRs, never dispatches.
# Reopening decisions require human/mayor judgment (see plugin.md).

set -uo pipefail
shopt -s nullglob

TOWN_ROOT="${GT_TOWN_ROOT:-$HOME/gt}"
RIGS_JSON_PATH="${TOWN_ROOT}/mayor/rigs.json"

log() { echo "[closed-bead-open-pr] $*"; }

if [ ! -f "$RIGS_JSON_PATH" ]; then
  log "SKIP: rigs.json not found"
  exit 0
fi

RIGS=$(jq -r '.rigs | keys[]' "$RIGS_JSON_PATH" 2>/dev/null)
if [ -z "$RIGS" ]; then
  log "SKIP: no rigs in rigs.json"
  exit 0
fi

FINDINGS=()

for RIG in $RIGS; do
  RIG_DIR="$TOWN_ROOT/$RIG"
  [ -d "$RIG_DIR" ] || continue

  # RIG_DIR itself is not a git checkout (it's the Gas Town rig root
  # containing mayor/rig, refinery/rig, polecats/*, etc. as separate
  # worktrees) - use refinery/rig, which every PR-merge-strategy rig has.
  REPO=$(git -C "$RIG_DIR/refinery/rig" remote get-url origin 2>/dev/null \
    | sed -E 's|.*github\.com[:/]||; s|\.git$||')
  [ -z "$REPO" ] && continue

  CANDIDATES=$(bd -C "$RIG_DIR" list --status closed --closed-after -48h \
    --title-contains "PR #" --exclude-label "flagged:closed-bead-open-pr" \
    --json --limit 200 2>/dev/null)
  [ -z "$CANDIDATES" ] && continue
  COUNT=$(echo "$CANDIDATES" | jq 'length' 2>/dev/null || echo 0)
  [ "$COUNT" -eq 0 ] && continue

  while IFS= read -r ISSUE; do
    [ -z "$ISSUE" ] && continue
    BEAD_ID=$(echo "$ISSUE" | jq -r '.id')
    TITLE=$(echo "$ISSUE" | jq -r '.title')
    PR_NUM=$(echo "$TITLE" | grep -oE 'PR #[0-9]+' | head -1 | grep -oE '[0-9]+')
    [ -z "$PR_NUM" ] && continue

    PR_STATE=$(gh pr view "$PR_NUM" --repo "$REPO" --json state \
      --jq '.state' 2>/dev/null || echo "UNKNOWN")

    if [ "$PR_STATE" = "OPEN" ]; then
      UNRESOLVED=$( (cd "$RIG_DIR" && gt refinery pr threads "$PR_NUM" \
        --unresolved --json 2>/dev/null) | jq 'length' 2>/dev/null || echo "?")
      FINDINGS+=("$RIG|$BEAD_ID|$PR_NUM|$UNRESOLVED|$TITLE")
      log "FLAG: $RIG/$BEAD_ID closed, PR #$PR_NUM still OPEN, $UNRESOLVED unresolved thread(s)"
    fi

    # Mark checked only once the PR state is actually known - a transient
    # gh failure (rate limit, auth, network) must not permanently hide a
    # real closed-bead/open-PR case behind this label.
    [ "$PR_STATE" != "UNKNOWN" ] && bd -C "$RIG_DIR" label add "$BEAD_ID" "flagged:closed-bead-open-pr" 2>/dev/null || true
  done < <(echo "$CANDIDATES" | jq -c '.[]')
done

log ""
log "=== ${#FINDINGS[@]} finding(s) ==="

if [ "${#FINDINGS[@]}" -gt 0 ]; then
  HAS_REAL=false
  BODY="rig|bead|PR|unresolved|title"$'\n'
  for F in "${FINDINGS[@]}"; do
    IFS='|' read -r RIG BEAD_ID PR_NUM UNRESOLVED TITLE <<< "$F"
    BODY+="$RIG|$BEAD_ID|#$PR_NUM|$UNRESOLVED|$TITLE"$'\n'
    if [ "$UNRESOLVED" != "0" ] && [ "$UNRESOLVED" != "?" ]; then
      HAS_REAL=true
    fi
  done

  SUBJECT="closed-bead-open-pr: ${#FINDINGS[@]} finding(s)"
  [ "$HAS_REAL" = true ] && SUBJECT="$SUBJECT — includes unresolved work"

  gt mail send mayor/ -s "$SUBJECT" --stdin <<< "$BODY"

  for F in "${FINDINGS[@]}"; do
    IFS='|' read -r RIG BEAD_ID PR_NUM UNRESOLVED TITLE <<< "$F"
    gt mail send "$RIG/witness" -s "closed-bead-open-pr: $BEAD_ID / PR #$PR_NUM ($UNRESOLVED unresolved)" --stdin <<BODY2
$BEAD_ID closed but PR #$PR_NUM is still open ($UNRESOLVED unresolved review
thread(s)). Visibility only, per the sweep pattern from 2026-08-09/10 - not
asking you to reopen or act. Mayor has the full table.
BODY2
  done
fi

SUMMARY="closed-bead-open-pr: ${#FINDINGS[@]} finding(s) this cycle"
_rid="$(bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:closed-bead-open-pr,result:success \
  -d "$SUMMARY" --silent 2>/dev/null)" || true
[ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true

log "$SUMMARY"
