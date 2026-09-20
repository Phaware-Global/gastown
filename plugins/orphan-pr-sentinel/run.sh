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
STATE_FILE="${GT_ORPHAN_PR_SENTINEL_STATE:-$TOWN_ROOT/.orphan-pr-sentinel-state.json}"
DRY_RUN="${DRY_RUN:-0}"
ERRORS=0

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
    --json number,title,statusCheckRollup,headRefOid --limit 100 2>/dev/null)
  if [ -z "$PRS" ]; then
    log "ERROR: gh pr list failed for $RIG ($REPO)"
    ERRORS=1
    continue
  fi
  PR_COUNT=$(echo "$PRS" | jq 'length' 2>/dev/null || echo 0)
  [ "$PR_COUNT" -eq 0 ] && continue

  while IFS= read -r PR_JSON; do
    [ -z "$PR_JSON" ] && continue
    PR_NUM=$(echo "$PR_JSON" | jq -r '.number')
    PR_TITLE=$(echo "$PR_JSON" | jq -r '.title')
    PR_HEAD=$(echo "$PR_JSON" | jq -r '.headRefOid // ""')

    if is_suppressed "$RIG" "$PR_NUM"; then
      continue
    fi

    # Does any OPEN bead in this rig reference "PR #<N>"? bd list defaults
    # to excluding closed issues. Match on a number boundary so "PR #23"
    # doesn't also match "PR #231", and treat a non-numeric result (bd
    # hang/error) as a query failure rather than "zero owners".
    OWNERS_JSON=$(bd -C "$RIG_DIR" list --title-contains "PR #$PR_NUM" --json --limit 0 2>/dev/null)
    OWNER_COUNT=$(echo "$OWNERS_JSON" | jq --arg n "$PR_NUM" \
      '[.[] | select((.title // "") | test("PR #" + $n + "([^0-9]|$)"; "i"))] | length' 2>/dev/null)
    if ! [[ "$OWNER_COUNT" =~ ^[0-9]+$ ]]; then
      log "ERROR: bd title query failed for $RIG PR #$PR_NUM"
      ERRORS=1
      continue
    fi
    if [ "$OWNER_COUNT" != "0" ]; then
      continue
    fi
    # Title match can miss a bead that only mentions the PR in its
    # description (e.g. "This is that bead" style follow-ups) - check too.
    DESC_OWNERS=$(bd -C "$RIG_DIR" list --desc-contains "PR #$PR_NUM" --json --limit 0 2>/dev/null)
    DESC_OWNER_COUNT=$(echo "$DESC_OWNERS" | jq --arg n "$PR_NUM" \
      '[.[] | select((.description // "") | test("PR #" + $n + "([^0-9]|$)"; "i"))] | length' 2>/dev/null)
    if ! [[ "$DESC_OWNER_COUNT" =~ ^[0-9]+$ ]]; then
      log "ERROR: bd desc query failed for $RIG PR #$PR_NUM"
      ERRORS=1
      continue
    fi
    if [ "$DESC_OWNER_COUNT" != "0" ]; then
      continue
    fi

    UNRESOLVED=$( (cd "$RIG_DIR" && gt refinery pr threads "$PR_NUM" \
      --unresolved --json 2>/dev/null) | jq 'length' 2>/dev/null || echo "?")
    if ! [[ "$UNRESOLVED" =~ ^[0-9]+$ ]]; then
      log "ERROR: threads query failed for $RIG PR #$PR_NUM"
      ERRORS=1
    fi
    CI_FAILING=$(echo "$PR_JSON" | jq '[.statusCheckRollup[]? | select(
      .conclusion == "FAILURE" or .conclusion == "CANCELLED" or
      .conclusion == "TIMED_OUT" or .state == "FAILURE" or .state == "ERROR"
    )] | length > 0' 2>/dev/null || echo "false")

    if { [ "$UNRESOLVED" != "0" ] && [ "$UNRESOLVED" != "?" ]; } || [ "$CI_FAILING" = "true" ]; then
      # Attacker-controlled title: this repo is public, so any GitHub user
      # can open a PR and its title is untrusted input to the mayor/witness
      # LLM inboxes below. Only forward the real title for a known insider;
      # withhold it for anyone else. Checked only for actual candidates
      # (post ownership/unresolved filtering), not every open PR.
      PR_ASSOC=$(gh api "repos/$REPO/pulls/$PR_NUM" --jq '.author_association // "NONE"' 2>/dev/null)
      case "$PR_ASSOC" in
        OWNER|MEMBER|COLLABORATOR) ;;
        *) PR_TITLE="(external PR — title withheld, untrusted author)" ;;
      esac
      FINDINGS+=("$RIG|$PR_NUM|$UNRESOLVED|$CI_FAILING|$PR_HEAD|$PR_TITLE")
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
  STATE=$([ -f "$STATE_FILE" ] && cat "$STATE_FILE" 2>/dev/null)
  [ -z "$STATE" ] && STATE="{}"
  NOW=$(date +%s)

  BODY="rig|PR|unresolved|ci_failing|title"$'\n'
  MAIL_BODY="rig|PR|unresolved|ci_failing|title"$'\n'
  TO_MAIL=()
  for F in "${FINDINGS[@]}"; do
    IFS='|' read -r RIG PR_NUM UNRESOLVED CI_FAILING PR_HEAD TITLE <<< "$F"
    BODY+="$RIG|#$PR_NUM|$UNRESOLVED|$CI_FAILING|$TITLE"$'\n'

    # Dedupe per rig/PR/head SHA: only mail when the PR is new to state, its
    # head moved, or the last report is more than 24h old. Repeats are
    # logged but not mailed, so a stuck orphan doesn't spam a permanent
    # mail bead every cooldown cycle.
    KEY="$RIG:$PR_NUM"
    PREV_SHA=$(echo "$STATE" | jq -r --arg k "$KEY" '.[$k].sha // ""' 2>/dev/null)
    PREV_TS=$(echo "$STATE" | jq -r --arg k "$KEY" '.[$k].ts // 0' 2>/dev/null)
    [[ "$PREV_TS" =~ ^[0-9]+$ ]] || PREV_TS=0
    if [ -n "$PR_HEAD" ] && [ "$PREV_SHA" = "$PR_HEAD" ] && [ $(( NOW - PREV_TS )) -lt 86400 ]; then
      log "SKIP-DEDUPE: $RIG PR #$PR_NUM already reported at this head within 24h"
      continue
    fi
    MAIL_BODY+="$RIG|#$PR_NUM|$UNRESOLVED|$CI_FAILING|$TITLE"$'\n'
    TO_MAIL+=("$F")
  done

  if [ "$DRY_RUN" = "1" ]; then
    log "DRY_RUN=1 - not sending mail. Would have sent (full findings, dedupe not applied):"
    echo "$BODY"
  elif [ "${#TO_MAIL[@]}" -eq 0 ]; then
    log "All findings already reported recently - nothing new to mail"
  else
    # Only persist dedupe state once the mayor mail actually went out -
    # otherwise a transient send failure would silently mute these PRs for
    # 24h with nothing ever delivered.
    if gt mail send mayor/ -s "orphan-pr-sentinel: ${#TO_MAIL[@]} PR(s) with no owning bead" --stdin <<< "$MAIL_BODY"; then
      for F in "${TO_MAIL[@]}"; do
        IFS='|' read -r RIG PR_NUM UNRESOLVED CI_FAILING PR_HEAD TITLE <<< "$F"
        KEY="$RIG:$PR_NUM"
        if gt mail send "$RIG/witness" -s "orphan-pr-sentinel: PR #$PR_NUM has no open bead" --stdin <<BODY2
PR #$PR_NUM ($TITLE) is open with $UNRESOLVED unresolved thread(s)
(ci_failing=$CI_FAILING) and no open bead in this rig references it - that is
the verified FACT. It does NOT mean the PR is abandoned: it could be owned
out-of-band (e.g. by the overseer), which this check structurally cannot see.
Confirm nobody already has it in flight before filing/claiming - do not
dispatch on this alone. Visibility only. Mayor has the full table.
BODY2
        then
          STATE=$(echo "$STATE" | jq --arg k "$KEY" --arg sha "$PR_HEAD" --argjson ts "$NOW" \
            '.[$k] = {"sha": $sha, "ts": $ts}' 2>/dev/null)
        else
          log "ERROR: witness mail failed for $RIG PR #$PR_NUM"
          ERRORS=1
        fi
      done
      echo "$STATE" > "$STATE_FILE" 2>/dev/null || true
    else
      log "ERROR: gt mail send mayor/ failed - not persisting dedupe state this cycle"
      ERRORS=1
    fi
  fi
else
  if [ "$ERRORS" = "1" ]; then
    log "Incomplete: bd/gh query failures occurred this cycle - skipping 'Clean' determination"
  else
    log "Clean: every open PR with unresolved work or failing CI has an open bead owning it"
  fi
fi

if [ "$DRY_RUN" != "1" ]; then
  RESULT="success"
  [ "$ERRORS" = "1" ] && RESULT="failure"
  SUMMARY="orphan-pr-sentinel: ${#FINDINGS[@]} orphan(s) this cycle"
  _rid="$(bd create "$SUMMARY" -t chore --ephemeral \
    -l "type:plugin-run,plugin:orphan-pr-sentinel,result:$RESULT" \
    -d "$SUMMARY" --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
fi
