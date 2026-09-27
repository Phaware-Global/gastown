+++
name = "closed-bead-open-pr"
description = "Detect review-fix beads closed while their linked PR is still open (possible false closure)"
version = 1

[gate]
# HELD manual 2026-08-10 by deacon: mayor's 8-of-8 sweep (hq-wisp-csht0) that
# motivated this plugin was retracted (hq-wisp-6eww0) - most round-scoped
# review-fix beads closing while their PR stays open is NORMAL, not a
# defect. This plugin's v1 run.sh does not distinguish round-scoped beads
# and fired noisy findings/mail before the correction was read. DO NOT
# re-enable (cooldown) until run.sh is redesigned per mayor's narrower ask:
# flag only non-round beads, or beads whose PR has had no new commits since
# closure. See plugin.md body below for the outdated v1 design - needs a
# rewrite, not just a gate flip.
type = "manual"

[tracking]
labels = ["plugin:closed-bead-open-pr", "category:integrity"]
digest = true

[execution]
timeout = "3m"
notify_on_failure = true
severity = "high"
+++

# Closed Bead / Open PR Sentinel

Detects the "closed bead + open PR" pattern that let review-fix work silently
disappear on 2026-08-09/10: a review-fix bead (title mentions `PR #<N>`) gets
closed by `gt done` or similar, but the PR it targets is still open — often
with unresolved review threads. bd/`bd ready` then reads clean while the PR
sits abandoned. Sweep that night found 8 of 8 dispatched review-fix beads hit
this, 6 with real outstanding work.

**DETECT ONLY. Never reopen, never comment on the PR, never dispatch.**
Reopening decisions require human/mayor judgment — some closures are
deliberately superseded by a successor bead scoping the same PR, and
auto-reopening would create duplicate ownership of the same threads (the
dispatch-storm shape). This plugin's only job is to make the pattern visible.

## Step 1: Enumerate rigs with a GitHub remote

```bash
TOWN_ROOT="${GT_TOWN_ROOT:-$HOME/gt}"
RIGS_JSON_PATH="${TOWN_ROOT}/mayor/rigs.json"
log() { echo "[closed-bead-open-pr] $*"; }

if [ ! -f "$RIGS_JSON_PATH" ]; then
  log "SKIP: rigs.json not found"
  exit 0
fi

RIGS=$(jq -r '.rigs | keys[]' "$RIGS_JSON_PATH" 2>/dev/null)
```

## Step 2: Per rig, find closed beads mentioning a PR number

For each rig, skip if there's no GitHub remote. Otherwise pull closed beads
from the last 48h whose title mentions `PR #<N>` and that haven't already
been flagged by a previous run (tracked via a label on the bead itself, so
this doesn't re-alert every 30 minutes on the same finding).

```bash
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

    # Mark checked either way so the same bead isn't rescanned every cycle.
    bd -C "$RIG_DIR" label add "$BEAD_ID" "flagged:closed-bead-open-pr" 2>/dev/null || true
  done < <(echo "$CANDIDATES" | jq -c '.[]')
done
```

## Step 3: Report

If FINDINGS is empty, log "clean" and exit. Otherwise mail mayor with the
full table (rig, bead, PR, unresolved count, title) — same shape as the
manual sweep table — and mail each affected rig's witness so it shows up
locally too. Do not escalate automatically; this is a report, not an
incident on its own (mayor already knows the class exists and wants to see
occurrences, not be paged for each one). Bump severity in the mail subject
only if any finding has unresolved > 0.

```bash
if [ "${#FINDINGS[@]}" -eq 0 ]; then
  log "Clean: no closed-bead+open-PR findings this cycle"
else
  log ""
  log "=== ${#FINDINGS[@]} finding(s) ==="
  HAS_REAL=false
  BODY="rig|bead|PR|unresolved|title"$'\n'
  for F in "${FINDINGS[@]}"; do
    IFS='|' read -r RIG BEAD_ID PR_NUM UNRESOLVED TITLE <<< "$F"
    BODY+="$RIG|$BEAD_ID|#$PR_NUM|$UNRESOLVED|$TITLE"$'\n'
    [ "$UNRESOLVED" != "0" ] && [ "$UNRESOLVED" != "?" ] && HAS_REAL=true
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
```

## Record result

```bash
SUMMARY="closed-bead-open-pr: ${#FINDINGS[@]} finding(s) this cycle"
bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:closed-bead-open-pr,result:success \
  -d "$SUMMARY" --silent 2>/dev/null || true
```
