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

# Without this, a BUILD_ROOT that isn't a git repo yet (first run, or after
# invalid-worktree cleanup) lets git's upward directory search discover an
# enclosing repo (e.g. the town's umbrella .git) instead of failing closed.
# The identity checks below then treat that discovered repo as BUILD_ROOT,
# and fetch/checkout --force runs against it instead.
export GIT_CEILING_DIRECTORIES="$TOWN_ROOT"

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

  # mkdir is atomic, so this serializes the read-modify-write below against
  # an overlapping invocation (e.g. a manual --force run overlapping the
  # cooldown-gated automatic one) that would otherwise race and silently
  # drop a consecutive skip from the streak.
  local lockdir="${SKIP_COUNT_FILE}.lock"
  local waited=0
  while ! mkdir "$lockdir" 2>/dev/null; do
    waited=$((waited + 1))
    [ "$waited" -ge 50 ] && break # ~5s: give up on the lock rather than hang
    sleep 0.1
  done

  local count=0
  if [ -f "$SKIP_COUNT_FILE" ]; then
    count=$(cat "$SKIP_COUNT_FILE" 2>/dev/null || echo 0)
  fi
  # Guard against garbage (partial write, hand edit, or an injection payload)
  # before using it in arithmetic: unvalidated content can abort the script
  # under set -e while the lock above is still held, leaking it forever.
  case "$count" in
  '' | *[!0-9]*) count=0 ;;
  esac
  count=$((count + 1))
  echo "$count" > "$SKIP_COUNT_FILE" 2>/dev/null || true
  rmdir "$lockdir" 2>/dev/null || true

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
IS_SKIPPED=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('skipped', False))" 2>/dev/null || echo "False")
STALE_ERROR=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('error', ''))" 2>/dev/null || echo "")

# gt stale exits 0 with stale:false on both a skipped check and an internal
# error (repo not found, no build-branch ref) — reading only "stale" here
# would record these as "binary is fresh" and reset the skip streak.
if [ "$IS_SKIPPED" = "True" ] || [ -n "$STALE_ERROR" ]; then
  skip_while_stale "gt stale could not determine staleness: ${STALE_ERROR:-skipped}"
fi

if [ "$IS_STALE" != "True" ]; then
  log "Binary is fresh. Nothing to do."
  record_run "rebuild-gt: binary is fresh" "success"
  reset_skip_count
  exit 0
fi

# From here on gt stale THINKS the binary is stale, based on RIG_ROOT's
# possibly-lagging local knowledge of the build-branch ref (RIG_ROOT is never
# fetched by this script). The forward-only decision below is deliberately
# NOT taken from gt stale's "forward" field for the same reason: it's re-
# derived after the fresh fetch, from BUILD_ROOT's real origin/main HEAD and
# the binary actually installed right now, so a lagging RIG_ROOT view can
# only cost an extra fetch+compare — never a wrong build/skip/failure call.

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

# --- Forward-only check, against the binary actually installed right now ----
#
# Re-derives both sides fresh here (the commit really installed + BUILD_ROOT's
# real origin/main HEAD) instead of trusting gt stale's cached "forward"
# field, so a legitimate "nothing to do" or "would be a downgrade" outcome
# (which make safe-install's check-forward-only would also refuse) is never
# misreported as a build failure below.

BUILD_HEAD=$(git -C "$BUILD_ROOT" rev-parse HEAD)

# Taken from the already-fetched STALE_JSON's binary_commit rather than
# parsing `gt version`'s text output: that format doesn't always include an
# '@' (e.g. no named branch in cwd), and under `set -euo pipefail` a `grep`
# that finds no match aborts the whole script right here on every run.
INSTALLED_COMMIT=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('binary_commit', ''))" 2>/dev/null || echo "")

if ! [[ "$INSTALLED_COMMIT" =~ ^[0-9a-f]{7,40}$ ]]; then
  skip_while_stale "gt stale reported no valid binary_commit to compare against (got '${INSTALLED_COMMIT:-empty}')"
fi

case "$BUILD_HEAD" in
"$INSTALLED_COMMIT"*)
  log "Installed binary is already at origin/main ($BUILD_HEAD). Nothing to do."
  record_run "rebuild-gt: binary is fresh" "success"
  reset_skip_count
  exit 0
  ;;
esac
if ! git -C "$BUILD_ROOT" merge-base --is-ancestor "$INSTALLED_COMMIT" "$BUILD_HEAD" 2>/dev/null; then
  skip_while_stale "origin/main ($BUILD_HEAD) is not a forward descendant of the installed binary ($INSTALLED_COMMIT) — would be a downgrade"
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
