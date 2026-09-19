#!/usr/bin/env bash
# orphan-pr-sentinel/run.sh — Detect open PRs with no open bead owning them.
#
# v2 of closed-bead-open-pr (retired/manual, see that plugin's plugin.md),
# redesigned per mayor's inversion (hq-wisp-rvpxe, 2026-08-10): round-scoped
# review-fix beads closing while their PR stays open is NORMAL - the real
# gap is a PR that NO open bead references at all, sitting with unresolved
# threads or a failing check and nobody picking it up. Low-noise by
# construction: round closures never trigger this as long as one bead
# somewhere still owns the PR.
#
# DETECT ONLY. Never files a bead, never comments on the PR, never dispatches.
#
# Set DRY_RUN=1 to print findings without sending any mail (use this to
# validate before enabling the cooldown gate or after changing the script).

set -uo pipefail
shopt -s nullglob

TOWN_ROOT="${GT_TOWN_ROOT:-$HOME/gt}"
RIGS_JSON_PATH="${TOWN_ROOT}/mayor/rigs.json"
DRY_RUN="${DRY_RUN:-0}"

log() { echo "[orphan-pr-sentinel] $*"; }

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

# Known false positives: PRs owned by the overseer (or another out-of-band
# process) that will never be referenced by an open bead's "PR #<N>" text.
# Format: "rig:pr_number". Mayor directive 2026-08-16 (hq-wisp-0a3tv): the
# overseer owns the whole reviewer PR stack on gastown #175, #176, #178,
# #179, #180, #181 (note: #177 is deliberately NOT on this list - it's not
# part of the overseer's stack). Unsuppress when the overseer reports the
# stack landed.
#
# gastown #194, #195, #196: CI failing on 3 misspell lint errors (not the
# #191 flake, not out-of-band ownership) - diagnosed and re-flagged three
# cycles running with the same answer each time. Mayor directive: stop
# re-flagging. Unsuppress once the lint fixes land or someone claims them.
SUPPRESS_PRS=(
  "gastown:175" "gastown:176" "gastown:178"
  "gastown:179" "gastown:180" "gastown:181"
  "gastown:194" "gastown:195" "gastown:196"
)
is_suppressed() {
  local key="$1:$2"
  local s
  for s in "${SUPPRESS_PRS[@]}"; do
    [ "$s" = "$key" ] && return 0
  done
  return 1
}

for RIG in $RIGS; do
  RIG_DIR="$TOWN_ROOT/$RIG"
  [ -d "$RIG_DIR" ] || continue

  # RIG_DIR itself is not a git checkout - use refinery/rig, which every
  # PR-merge-strategy rig has as an actual worktree.
  REPO=$(git -C "$RIG_DIR/refinery/rig" remote get-url origin 2>/dev/null \
    | sed -E 's|.*github\.com[:/]||; s|\.git$||')
  [ -z "$REPO" ] && continue

  PRS=$(gh pr list --repo "$REPO" --state open \
    --json number,title,statusCheckRollup --limit 100 2>/dev/null)
  [ -z "$PRS" ] && continue
  PR_COUNT=$(echo "$PRS" | jq 'length' 2>/dev/null || echo 0)
  [ "$PR_COUNT" -eq 0 ] && continue

  while IFS= read -r PR_JSON; do
    [ -z "$PR_JSON" ] && continue
    PR_NUM=$(echo "$PR_JSON" | jq -r '.number')
    PR_TITLE=$(echo "$PR_JSON" | jq -r '.title')

    if is_suppressed "$RIG" "$PR_NUM"; then
      continue
    fi

    # Does any OPEN bead in this rig reference "PR #<N>"? bd list defaults
    # to excluding closed issues.
    OWNERS_JSON=$(bd -C "$RIG_DIR" list --title-contains "PR #$PR_NUM" --json --limit 5 2>/dev/null)
    OWNER_COUNT=$(echo "$OWNERS_JSON" | jq 'length' 2>/dev/null || echo 0)
    if [ "$OWNER_COUNT" != "0" ]; then
      continue
    fi
    # Title match can miss a bead that only mentions the PR in its
    # description (e.g. "This is that bead" style follow-ups) - check too.
    DESC_OWNERS=$(bd -C "$RIG_DIR" list --desc-contains "PR #$PR_NUM" --json --limit 5 2>/dev/null)
    DESC_OWNER_COUNT=$(echo "$DESC_OWNERS" | jq 'length' 2>/dev/null || echo 0)
    if [ "$DESC_OWNER_COUNT" != "0" ]; then
      continue
    fi

    UNRESOLVED=$( (cd "$RIG_DIR" && gt refinery pr threads "$PR_NUM" \
      --unresolved --json 2>/dev/null) | jq 'length' 2>/dev/null || echo "?")
    CI_FAILING=$(echo "$PR_JSON" | jq '[.statusCheckRollup[]? | select(
      .conclusion == "FAILURE" or .conclusion == "CANCELLED" or
      .conclusion == "TIMED_OUT" or .state == "FAILURE" or .state == "ERROR"
    )] | length > 0' 2>/dev/null || echo "false")

    if { [ "$UNRESOLVED" != "0" ] && [ "$UNRESOLVED" != "?" ]; } || [ "$CI_FAILING" = "true" ]; then
      FINDINGS+=("$RIG|$PR_NUM|$UNRESOLVED|$CI_FAILING|$PR_TITLE")
      # Report the FACT ("no bead references it"), not the conclusion
      # ("orphaned") - the PR may still be owned out-of-band (e.g. by the
      # overseer), which this check structurally cannot see. Mayor
      # correction 2026-08-16 (hq-wisp-0a3tv) after a same-day near-miss
      # where "orphan" read as "unowned/abandoned" led to a duplicate
      # dispatch onto a PR the overseer was actively rebasing.
      log "NO-BEAD: $RIG PR #$PR_NUM — no open bead references it (may still be owned out-of-band), $UNRESOLVED unresolved thread(s), ci_failing=$CI_FAILING — $PR_TITLE"
    fi
  done < <(echo "$PRS" | jq -c '.[]')
done

log ""
log "=== ${#FINDINGS[@]} PR(s) with no owning bead (not necessarily orphaned) ==="

if [ "${#FINDINGS[@]}" -gt 0 ]; then
  BODY="rig|PR|unresolved|ci_failing|title"$'\n'
  for F in "${FINDINGS[@]}"; do
    IFS='|' read -r RIG PR_NUM UNRESOLVED CI_FAILING TITLE <<< "$F"
    BODY+="$RIG|#$PR_NUM|$UNRESOLVED|$CI_FAILING|$TITLE"$'\n'
  done

  if [ "$DRY_RUN" = "1" ]; then
    log "DRY_RUN=1 - not sending mail. Would have sent:"
    echo "$BODY"
  else
    gt mail send mayor/ -s "orphan-pr-sentinel: ${#FINDINGS[@]} PR(s) with no owning bead" --stdin <<< "$BODY"
    for F in "${FINDINGS[@]}"; do
      IFS='|' read -r RIG PR_NUM UNRESOLVED CI_FAILING TITLE <<< "$F"
      gt mail send "$RIG/witness" -s "orphan-pr-sentinel: PR #$PR_NUM has no open bead" --stdin <<BODY2
PR #$PR_NUM ($TITLE) is open with $UNRESOLVED unresolved thread(s)
(ci_failing=$CI_FAILING) and no open bead in this rig references it - that is
the verified FACT. It does NOT mean the PR is abandoned: it could be owned
out-of-band (e.g. by the overseer), which this check structurally cannot see.
Confirm nobody already has it in flight before filing/claiming - do not
dispatch on this alone. Visibility only. Mayor has the full table.
BODY2
    done
  fi
else
  log "Clean: every open PR with unresolved work or failing CI has an open bead owning it"
fi

if [ "$DRY_RUN" != "1" ]; then
  SUMMARY="orphan-pr-sentinel: ${#FINDINGS[@]} orphan(s) this cycle"
  bd create "$SUMMARY" -t chore --ephemeral \
    -l type:plugin-run,plugin:orphan-pr-sentinel,result:success \
    -d "$SUMMARY" --silent 2>/dev/null || true
fi
