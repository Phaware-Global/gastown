#!/usr/bin/env bash
# rebuild-gt/run.sh — Rebuild gt binary from gastown source if stale.
#
# SAFETY: Only rebuilds forward (binary is ancestor of HEAD) and only
# from main branch. A bad rebuild caused a crash loop (every session's
# startup hook failed, witness respawned, loop repeated every 1-2 min).
#
# Builds happen in a DEDICATED worktree (BUILD_ROOT below), never in the
# mayor's clone (RIG_ROOT). RIG_ROOT routinely carries beads-state edits
# and a local feature branch — it's the mayor's working clone, not a build
# checkout — so gating the rebuild on RIG_ROOT being clean and on main
# meant the rebuild silently skipped for hours at a time. BUILD_ROOT is
# reset to origin/main on every run regardless of RIG_ROOT's state.

set -euo pipefail

TOWN_ROOT="${GT_TOWN_ROOT:-$(gt town root 2>/dev/null)}"
RIG_ROOT="${TOWN_ROOT}/gastown/mayor/rig"
BUILD_ROOT="${TOWN_ROOT}/gastown/.gt-build"
SKIP_COUNT_FILE="${TOWN_ROOT}/gastown/.gt-build-skip-count"

# After this many consecutive skipped-while-stale runs, escalate instead of
# only writing the (easy to miss) ephemeral receipt. At the plugin's 1h
# cooldown gate, 3 consecutive skips is ~3h of undetected staleness — the
# original incident this bead fixes ran silently for hours.
SKIP_ESCALATE_THRESHOLD=3

log() { echo "[rebuild-gt] $*"; }

# record_run TITLE RESULT [DETAIL] — write the ephemeral plugin-run receipt.
record_run() {
  local title="$1" result="$2" detail="${3:-}"
  local _rid
  if [ -n "$detail" ]; then
    _rid="$(bd create "$title" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:"$result" \
      -d "$detail" --silent 2>/dev/null)" || true
  else
    _rid="$(bd create "$title" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:"$result" \
      --silent 2>/dev/null)" || true
  fi
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
}

reset_skip_count() {
  rm -f "$SKIP_COUNT_FILE" 2>/dev/null || true
}

# skip_while_stale REASON — record the skip, escalate after N consecutive
# skips, and exit 0. Only call this once IS_STALE has been confirmed True.
skip_while_stale() {
  local reason="$1"
  log "Skipping rebuild: $reason"
  record_run "Plugin: rebuild-gt [skipped]" "skipped" "Skipped: $reason"

  local count=0
  if [ -f "$SKIP_COUNT_FILE" ]; then
    count=$(cat "$SKIP_COUNT_FILE" 2>/dev/null || echo 0)
  fi
  count=$((count + 1))
  echo "$count" > "$SKIP_COUNT_FILE" 2>/dev/null || true

  if [ $((count % SKIP_ESCALATE_THRESHOLD)) -eq 0 ]; then
    gt escalate "rebuild-gt: binary stale for $count consecutive skipped runs" -s high \
      --reason "Latest skip: $reason. The gt binary has been stale for $count consecutive rebuild-gt runs without a successful rebuild." \
      2>/dev/null || true
  fi
  exit 0
}

# --- Detection ---------------------------------------------------------------

log "Checking binary staleness..."
STALE_JSON=$(gt stale --json 2>/dev/null) || {
  log "gt stale --json failed, skipping"
  exit 0
}

IS_STALE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('stale', False))" 2>/dev/null || echo "False")

if [ "$IS_STALE" != "True" ]; then
  log "Binary is fresh. Nothing to do."
  record_run "rebuild-gt: binary is fresh" "success"
  reset_skip_count
  exit 0
fi

# From here on the binary IS stale: every exit below is either a successful
# rebuild or a skip that must be recorded via skip_while_stale.

IS_FORWARD=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('forward', False))" 2>/dev/null || echo "False")

if [ "$IS_FORWARD" != "True" ]; then
  # NOTE: deliberately not gating on gt stale's on_main_branch/safe_to_rebuild
  # here — those reflect RIG_ROOT's checked-out branch, which is irrelevant
  # now that we always build from a fresh origin/main checkout in BUILD_ROOT.
  # "forward" itself is already computed against origin/main regardless of
  # what branch RIG_ROOT happens to be on.
  skip_while_stale "origin/main is not a forward descendant of the installed binary (would be a downgrade)"
fi

# --- Pre-flight: dedicated build worktree ------------------------------------

if ! git -C "$RIG_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  skip_while_stale "source repo $RIG_ROOT not found"
fi

log "Preparing dedicated build worktree at $BUILD_ROOT..."
git -C "$RIG_ROOT" worktree prune >/dev/null 2>&1 || true

if [ -d "$BUILD_ROOT" ] && ! git -C "$BUILD_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  log "Removing invalid build worktree directory $BUILD_ROOT"
  rm -rf "$BUILD_ROOT"
fi

if [ ! -d "$BUILD_ROOT" ]; then
  if ! git -C "$RIG_ROOT" worktree add --detach "$BUILD_ROOT" >/dev/null 2>&1; then
    skip_while_stale "failed to create build worktree at $BUILD_ROOT"
  fi
fi

if ! git -C "$BUILD_ROOT" fetch origin main --quiet; then
  skip_while_stale "failed to fetch origin/main into build worktree"
fi

if ! git -C "$BUILD_ROOT" checkout --detach --force origin/main --quiet; then
  skip_while_stale "failed to checkout origin/main in build worktree"
fi

# --- Build -------------------------------------------------------------------

OLD_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
log "Rebuilding gt from $BUILD_ROOT (origin/main)..."

if (cd "$BUILD_ROOT" && make build && make safe-install) 2>&1; then
  NEW_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
  log "Rebuilt: $OLD_VER -> $NEW_VER"
  record_run "rebuild-gt: $OLD_VER -> $NEW_VER" "success"
  reset_skip_count
else
  ERROR="make build/safe-install failed"
  log "FAILED: $ERROR"
  record_run "Plugin: rebuild-gt [failure]" "failure" "Build failed: $ERROR"
  gt escalate "Plugin FAILED: rebuild-gt" -s medium 2>/dev/null || true
  exit 1
fi
