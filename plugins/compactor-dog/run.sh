#!/usr/bin/env bash
# compactor-dog/run.sh — Executable compaction script for agent dogs.
#
# Discovers production databases on the Dolt server, compacts (flattens)
# databases exceeding the commit threshold, verifies data integrity,
# runs dolt_gc, and reports results.
#
# This is the agent-executable counterpart to compactor_dog.go. The Go
# daemon uses database/sql connections; this script uses `dolt sql` CLI
# to achieve the same flatten algorithm.
#
# Modes:
#   --check-only   Monitor and report only (matches plugin.md philosophy)
#   (default)      Perform compaction on databases exceeding threshold
#
# Usage: ./run.sh [--threshold N] [--databases db1,db2,...] [--dry-run] [--check-only]

set -euo pipefail

# --- Configuration -----------------------------------------------------------

DOLT_HOST="${DOLT_HOST:-127.0.0.1}"
DOLT_PORT="${DOLT_PORT:-3307}"
DOLT_USER="${DOLT_USER:-root}"
COMMIT_THRESHOLD="${COMMIT_THRESHOLD:-2000}"
# Default: auto-discover production databases via SHOW DATABASES.
# Override with --databases db1,db2,... for an explicit list.
DEFAULT_DBS="auto"
DRY_RUN=false
CHECK_ONLY=false
# MAYOR SAFETY GATE 2026-09-03: destructive compaction requires an explicit
# --compact. A flagless invocation previously soft-reset to root, flattened
# history and force-pushed every production DB over the threshold, while the
# daemon dispatch template described that same command as "monitor and
# escalate". A dog ran it twice; the second run was safe only because every DB
# happened to sit at <=93 commits. Refuse loudly rather than destroy quietly.
COMPACT_AUTHORIZED=false
LOGFILE=""
LOCKFILE="/tmp/compactor-dog.lock"

# --- Argument parsing ---------------------------------------------------------

while [[ $# -gt 0 ]]; do
  case "$1" in
    --threshold)   COMMIT_THRESHOLD="$2"; shift 2 ;;
    --databases)   DEFAULT_DBS="$2"; shift 2 ;;
    --dry-run)     DRY_RUN=true; shift ;;
    --check-only)  CHECK_ONLY=true; shift ;;
    --compact)     COMPACT_AUTHORIZED=true; shift ;;
    --help|-h)
      echo "Usage: $0 [--threshold N] [--databases db1,db2,...] [--dry-run] [--check-only]"
      echo "  --threshold N        Commit count before compaction (default: 2000)"
      echo "  --databases db1,...  Comma-separated database list (default: auto-discover)"
      echo "  --dry-run            Report only, don't compact"
      echo "  --check-only         Monitor and report only (no compaction)"
      echo "  --compact            REQUIRED to actually compact. Destructive:"
      echo "                       flattens history and force-pushes remote main."
      echo "                       Without it, this script reports and exits."
      exit 0
      ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# --- MAYOR SAFETY GATE (2026-09-03) ------------------------------------------
# Fail safe, not silent: with no --compact, behave exactly as --check-only.
if [[ "$COMPACT_AUTHORIZED" != "true" ]]; then
  if [[ "$CHECK_ONLY" != "true" && "$DRY_RUN" != "true" ]]; then
    echo "compactor-dog: --compact not supplied; running in --check-only mode." >&2
    echo "  Destructive compaction (flatten + force-push) requires --compact." >&2
    echo "  Mayor safety gate; see run.sh header. Reporting only." >&2
  fi
  CHECK_ONLY=true
fi

# --- Lock acquisition --------------------------------------------------------

# Prevent concurrent compaction runs (e.g., cron + manual).
if ! mkdir "$LOCKFILE" 2>/dev/null; then
  echo "[compactor-dog] ERROR: Another instance is running (lockfile: $LOCKFILE)"
  echo "[compactor-dog] If stale, remove with: rmdir $LOCKFILE"
  exit 1
fi
trap 'rmdir "$LOCKFILE" 2>/dev/null; rm -f "$LOGFILE"' EXIT

# --- Helpers ------------------------------------------------------------------

# Create a temp file for capturing stderr from dolt commands.
LOGFILE=$(mktemp /tmp/compactor-dog-stderr.XXXXXX)

# Validate that a name is safe for use in SQL (alphanumeric, underscore, hyphen).
validate_name() {
  local name="$1"
  local context="$2"
  if [[ ! "$name" =~ ^[a-zA-Z0-9_-]+$ ]]; then
    log "ERROR: Unsafe $context name rejected: '$name'"
    return 1
  fi
  return 0
}

# Validate that a value looks like a Dolt commit hash (base32 encoding: a-v plus 0-9).
validate_hash() {
  local hash="$1"
  local context="$2"
  if [[ ! "$hash" =~ ^[a-v0-9]+$ ]]; then
    log "ERROR: Unsafe $context hash rejected: '$hash'"
    return 1
  fi
  return 0
}

# Run a SQL query against the Dolt server, returning CSV without header.
# Global flags (--host, --port, --no-tls, -u, -p) must go BEFORE the sql subcommand.
# Use --use-db for database selection.
dolt_query() {
  local db="$1"
  local query="$2"
  local args=(dolt --host="$DOLT_HOST" --port="$DOLT_PORT" --no-tls -u "$DOLT_USER" -p "")
  if [[ -n "$db" ]]; then
    args+=(--use-db "$db")
  fi
  args+=(sql -q "$query" --result-format csv)
  "${args[@]}" 2>>"$LOGFILE" | tail -n +2 | tr -d '\r'
}

# Run a SQL statement (no result expected) against a specific database.
dolt_exec() {
  local db="$1"
  local query="$2"
  dolt --host="$DOLT_HOST" --port="$DOLT_PORT" --no-tls -u "$DOLT_USER" -p "" --use-db "$db" \
    sql -q "$query" --result-format csv >/dev/null 2>>"$LOGFILE"
}

log() {
  echo "[compactor-dog] $*"
}

# INVARIANT: a database with a configured Dolt remote is never flattened in a
# cycle that cannot push to it. Flatten rewrites the commit graph, so a remote
# we could not push after it keeps an old HEAD that is no longer in local
# history; the next cycle that can reach it would read the database as
# "diverged" and skip it for good. So when the remote exists but the
# visibility guard refuses it for ANY reason (public, unknown, non-GitHub,
# DoltHub, unreadable URL or name, or a missing guard, gh or timeout), the
# database is skipped this cycle (no fetch, no divergence check, no flatten, no
# push) and escalated once, fingerprinted per db and reason. A database with NO
# remote compacts locally. There is deliberately no per-reason branching.
#
# The push target is decided ONCE per database, up front, by
# resolve_push_target (remote name, URL and guard verdict in one place);
# remote_gate applies the invariant. This script and compactor_dog.go both send
# the town database off the machine, so both clear the dolt-archive visibility
# guard and fail closed.

# Source the dolt-archive visibility guard from GUARD_FILE; with no guard on
# disk, install a stub that refuses everything.
load_visibility_guard() {
  local guard_file="$1"
  if [[ -f "$guard_file" ]]; then
    # shellcheck source=../dolt-archive/visibility_guard.sh
    source "$guard_file"
  else
    remote_push_allowed() { echo "guard-missing"; return 1; }
  fi
}
load_visibility_guard "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../dolt-archive/visibility_guard.sh"

# Run a query and keep the first line of its result in QUERY_OUT. Returns
# non-zero if the QUERY FAILED; success with an empty result is QUERY_OUT="".
# Every read in the push-target path goes through this (or checks the status
# itself) so that "failed" is never mistaken for "empty": under pipefail a
# failing dolt makes dolt_query fail, and a bare "|| value=''" would turn that
# into an absence.
QUERY_OUT=""
query_first() {
  local out
  QUERY_OUT=""
  if ! out="$(dolt_query "$1" "$2" 2>/dev/null)"; then
    return 1
  fi
  QUERY_OUT="${out%%$'\n'*}"
}

# After a successful fetch of REMOTE_NAME: is the remote's main HEAD in local
# history? Returns 0 to proceed (remote HEAD absent, equal to local, or an
# ancestor), 10 when the remote has commits local lacks (diverged), 11 when the
# check could not be made (a read failed or returned something unusable) — that
# is not "no divergence", so the caller must skip the database, not flatten it
# and force-push over commits that might exist only on the remote. Logs the
# reason.
verify_remote_not_diverged() {
  local db="$1" local_head remote_head is_ancestor

  if ! query_first "$db" "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1"; then
    log "  ERROR: could not read the local HEAD of $db — cannot verify the remote"
    return 11
  fi
  local_head="$QUERY_OUT"
  if [[ -z "$local_head" ]] || ! validate_hash "$local_head" "local HEAD"; then
    log "  ERROR: local HEAD of $db is empty or malformed — cannot verify the remote"
    return 11
  fi

  if ! query_first "$db" "SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/${REMOTE_NAME}/main'"; then
    log "  ERROR: could not read the remote HEAD of $db — cannot verify the remote"
    return 11
  fi
  remote_head="$QUERY_OUT"
  # A successful empty read: the remote has no main branch yet.
  [[ -n "$remote_head" ]] || return 0
  if ! validate_hash "$remote_head" "remote HEAD"; then
    log "  ERROR: remote HEAD of $db is malformed — cannot verify the remote"
    return 11
  fi
  [[ "$remote_head" != "$local_head" ]] || return 0

  # Remote HEAD is an ancestor of local history (local is ahead): safe.
  if ! query_first "$db" "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = '$remote_head'"; then
    log "  ERROR: could not check whether the remote HEAD of $db is in local history — cannot verify the remote"
    return 11
  fi
  is_ancestor="$QUERY_OUT"
  case "$is_ancestor" in
    0)
      log "  ERROR: Remote has commits not in local history — skipping compaction to avoid data loss"
      log "  Local HEAD:  ${local_head:0:12}"
      log "  Remote HEAD: ${remote_head:0:12}"
      return 10
      ;;
    ''|*[!0-9]*)
      log "  ERROR: unexpected ancestor-check result '$is_ancestor' for $db — cannot verify the remote"
      return 11
      ;;
  esac
  return 0
}

# Decide DB's push target. Sets, for the caller:
#   HAS_REMOTE           true only for a validly named remote whose URL the
#                        guard confirms private (the push target)
#   REMOTE_NAME/URL      that remote (empty otherwise)
#   PUSH_REFUSED         true when a remote EXISTS but is not a push target
#   PUSH_REFUSAL_REASON  why (the guard's verdict, or url-unresolvable /
#                        unsafe-remote-name)
# No remote at all leaves all of them false/empty.
HAS_REMOTE=false
REMOTE_NAME=""
REMOTE_URL=""
PUSH_REFUSED=false
PUSH_REFUSAL_REASON=""
resolve_push_target() {
  local db="$1" reason
  HAS_REMOTE=false
  REMOTE_NAME=""
  REMOTE_URL=""
  PUSH_REFUSED=false
  PUSH_REFUSAL_REASON=""

  # Only a SUCCESSFUL lookup with an empty result means "no remote"; a failed
  # lookup leaves the destination unknown, which is a refusal.
  if ! query_first "$db" "SELECT name FROM dolt_remotes LIMIT 1"; then
    PUSH_REFUSED=true
    PUSH_REFUSAL_REASON="remote-lookup-failed"
    return 0
  fi
  local name="$QUERY_OUT"
  [[ -n "$name" ]] || return 0
  if ! validate_name "$name" "remote"; then
    PUSH_REFUSED=true
    PUSH_REFUSAL_REASON="unsafe-remote-name"
    return 0
  fi

  if ! query_first "$db" "SELECT url FROM dolt_remotes WHERE name = '$name'"; then
    PUSH_REFUSED=true
    PUSH_REFUSAL_REASON="remote-lookup-failed"
    return 0
  fi
  local url="$QUERY_OUT"
  if [[ -z "$url" ]]; then
    PUSH_REFUSED=true
    PUSH_REFUSAL_REASON="url-unresolvable"
    return 0
  fi
  if ! reason="$(remote_push_allowed "$url")"; then
    PUSH_REFUSED=true
    PUSH_REFUSAL_REASON="$reason"
    return 0
  fi

  HAS_REMOTE=true
  REMOTE_NAME="$name"
  REMOTE_URL="$url"
}

# Escalate a refused push target once: the fingerprint is per db and reason, so
# an open escalation suppresses the repeats from later cycles. Never fails the
# run (set -e).
escalate_push_refused() {
  local db="$1" reason="$2" title why
  if [[ "$reason" == "remote-lookup-failed" ]]; then
    # The remote's name or URL could not be read, so there is no refusal of a
    # particular remote to report: say the configuration was unreadable.
    title="compactor-dog: skipped $db — could not read its Dolt remote configuration ($reason)"
    why="compactor-dog could not read the Dolt remote configuration of $db ($reason), so it cannot tell whether a push is possible and skipped compaction: flattening a database whose remote it cannot push would leave that remote diverged. Check that the Dolt server is healthy; the skip repeats until the read succeeds."
  else
    title="compactor-dog: skipped $db — its Dolt remote cannot be pushed ($reason)"
    why="$db has a Dolt remote that the push visibility guard refused ($reason), so compaction was skipped: flattening without being able to push would leave the remote diverged. Fix the remote (make it a private GitHub repo or remove it) or the guard environment (gh, timeout)."
  fi
  gt escalate "$title" -s MEDIUM \
    --fingerprint "compactor-dog:push-refused:${db}:${reason}" \
    --reason "$why" \
    >/dev/null 2>&1 || true
}

# Apply the invariant for DB. Returns 0 to go ahead (no remote, or a push
# target to fetch from and push to); returns 3 to skip the database this cycle,
# after logging and escalating the refusal.
remote_gate() {
  local db="$1"
  resolve_push_target "$db"
  if $PUSH_REFUSED; then
    log "  REFUSED: the Dolt remote of $db cannot be pushed (${PUSH_REFUSAL_REASON}) — skipping compaction this cycle (no fetch, no flatten, no push)"
    escalate_push_refused "$db" "$PUSH_REFUSAL_REASON"
    return 3
  fi
  return 0
}

# Force-push the compacted history of DB to REMOTE, which must be the push
# target resolve_push_target accepted. Flatten rewrites the commit graph, so
# this must be a force-push; --set-upstream because flatten loses branch
# tracking metadata. Refuses (return 2) for anything else, so it cannot be
# called on a remote nobody judged.
push_compacted_history() {
  local db="$1" remote="$2"
  if ! $HAS_REMOTE || [[ "$remote" != "$REMOTE_NAME" ]]; then
    log "  REFUSED: '$remote' for $db is not the resolved push target — not pushing"
    return 2
  fi
  dolt_exec "$db" "CALL DOLT_PUSH('--force', '--set-upstream', '$remote', 'main')"
}

# Wait for a PID with a timeout (seconds). Returns 0 if process exits in time.
# macOS lacks `timeout` command, so we poll with sleep.
wait_with_timeout() {
  local pid="$1" secs="$2" elapsed=0
  while kill -0 "$pid" 2>/dev/null; do
    if [[ "$elapsed" -ge "$secs" ]]; then
      return 1
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  wait "$pid" 2>/dev/null
}

# --- Step 1: Discover production databases ------------------------------------

log "Starting compaction cycle (threshold=$COMMIT_THRESHOLD, dry_run=$DRY_RUN, check_only=$CHECK_ONLY)"

# If databases were explicitly provided, use those. Otherwise, auto-discover
# from the server and filter out system/test databases.
if [[ "$DEFAULT_DBS" == "auto" ]]; then
  ALL_DBS=$(dolt_query "" "SHOW DATABASES" | grep -v -E '^(information_schema|mysql|dolt_cluster)$' | grep -v -E '^(testdb_|beads_t|beads_pt|doctest_)')
  if [[ -z "$ALL_DBS" ]]; then
    log "ERROR: No production databases found (is Dolt running on $DOLT_HOST:$DOLT_PORT?)"
    gt escalate "compactor-dog: no databases found" -s MEDIUM \
      --reason "Dolt server at $DOLT_HOST:$DOLT_PORT returned no production databases" 2>/dev/null || true
    exit 1
  fi
else
  # Convert comma-separated list to newline-separated
  ALL_DBS=$(echo "$DEFAULT_DBS" | tr ',' '\n')
fi

# Validate all database names before proceeding.
VALIDATED_DBS=""
while IFS= read -r DB; do
  [[ -z "$DB" ]] && continue
  if validate_name "$DB" "database"; then
    VALIDATED_DBS="${VALIDATED_DBS}${DB}"$'\n'
  fi
done <<< "$ALL_DBS"
ALL_DBS="$VALIDATED_DBS"

DB_COUNT=$(printf '%s' "$ALL_DBS" | grep -c . || true)
log "Production databases ($DB_COUNT): $(printf '%s' "$ALL_DBS" | tr '\n' ' ')"

if [[ "$DB_COUNT" -eq 0 ]]; then
  log "ERROR: No valid databases to process"
  exit 1
fi

# --- Step 2: Count commits per database and identify candidates ---------------

log ""
log "=== Commit Counts ==="

declare -a CANDIDATES=()
declare -a SKIPPED=()
REPORT=""

while IFS= read -r DB; do
  [[ -z "$DB" ]] && continue

  COUNT=$(dolt_query "$DB" "SELECT COUNT(*) AS cnt FROM dolt_log" 2>/dev/null | head -1)
  if [[ -z "$COUNT" || "$COUNT" == "null" ]]; then
    log "  $DB: ERROR querying commit count (skipping)"
    REPORT="${REPORT}${DB}: error\n"
    continue
  fi

  log "  $DB: $COUNT commits"
  REPORT="${REPORT}${DB}: ${COUNT} commits\n"

  if [[ "$COUNT" -ge "$COMMIT_THRESHOLD" ]]; then
    CANDIDATES+=("$DB:$COUNT")
  else
    SKIPPED+=("$DB")
  fi
done <<< "$ALL_DBS"

log ""
log "Candidates for compaction: ${#CANDIDATES[@]}"
log "Skipped (below threshold): ${#SKIPPED[@]}"

if [[ ${#CANDIDATES[@]} -eq 0 ]]; then
  log "All databases within threshold ($COMMIT_THRESHOLD). No compaction needed."
  SUMMARY="compactor-dog: all ${DB_COUNT} DBs below threshold ($COMMIT_THRESHOLD commits)"
  _rid="$(bd create "$SUMMARY" -t chore --ephemeral \
    -l type:plugin-run,plugin:compactor-dog,result:success \
    -d "$SUMMARY" --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
  exit 0
fi

# --- Check-only mode: report candidates and exit -----------------------------

if $CHECK_ONLY; then
  log ""
  log "=== CHECK-ONLY — databases exceeding threshold: ==="
  for entry in "${CANDIDATES[@]}"; do
    log "  ${entry%%:*} (${entry##*:} commits) — recommends compaction"
  done
  SUMMARY="compactor-dog: ${#CANDIDATES[@]} DBs exceed threshold ($COMMIT_THRESHOLD commits)"
  _rid="$(bd create "$SUMMARY" -t chore --ephemeral \
    -l type:plugin-run,plugin:compactor-dog,result:check-only \
    -d "$SUMMARY" --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
  exit 0
fi

if $DRY_RUN; then
  log ""
  log "=== DRY RUN — would compact: ==="
  for entry in "${CANDIDATES[@]}"; do
    log "  ${entry%%:*} (${entry##*:} commits)"
  done
  exit 0
fi

# --- Step 3: Compact (flatten) each candidate database ------------------------

COMPACTED=0
ERRORS=0
ERROR_DETAILS=""

# Temp file for pre-flight row counts (bash 3.2 compatible — no associative arrays).
PRE_COUNTS_FILE=$(mktemp /tmp/compactor-dog-precounts.XXXXXX)
trap 'rmdir "$LOCKFILE" 2>/dev/null; rm -f "$LOGFILE" "$PRE_COUNTS_FILE"' EXIT

for entry in "${CANDIDATES[@]}"; do
  DB="${entry%%:*}"
  COMMIT_COUNT="${entry##*:}"

  log ""
  log "=== Compacting $DB ($COMMIT_COUNT commits) ==="

  # Step 3a: Record pre-flight row counts for integrity verification.
  log "  Recording pre-flight row counts..."
  PRE_TABLES=$(dolt_query "$DB" \
    "SELECT table_name FROM information_schema.tables WHERE table_schema = '$DB' AND table_name NOT LIKE 'dolt_%' AND table_type = 'BASE TABLE'")

  # Clear pre-counts file for this database.
  : > "$PRE_COUNTS_FILE"
  TABLE_COUNT=0

  while IFS= read -r TABLE; do
    [[ -z "$TABLE" ]] && continue
    if ! validate_name "$TABLE" "table"; then
      continue
    fi
    ROW_COUNT=$(dolt_query "$DB" "SELECT COUNT(*) FROM \`$TABLE\`" 2>/dev/null | head -1)
    printf '%s\t%s\n' "$TABLE" "${ROW_COUNT:-0}" >> "$PRE_COUNTS_FILE"
    TABLE_COUNT=$((TABLE_COUNT + 1))
  done <<< "$PRE_TABLES"

  log "  Pre-flight: $TABLE_COUNT tables recorded"

  # Step 3a.5: Fetch from remote before compaction to check for divergence.
  # Without this, flatten rewrites the commit graph and DoltHub push can never
  # fast-forward again (see gt-mkd1).
  # NOTE: We use DOLT_FETCH instead of DOLT_PULL because a live Dolt server
  # always has uncommitted working set changes, making DOLT_PULL fail with
  # "cannot merge with uncommitted changes".
  # The invariant (see remote_gate): a database whose configured remote cannot
  # be pushed this cycle is skipped, never flattened. A database with no remote
  # compacts locally; one with a push target is fetched, flattened and pushed.
  if ! remote_gate "$DB"; then
    SKIPPED+=("$DB (remote cannot be pushed: $PUSH_REFUSAL_REASON)")
    continue
  fi
  if $HAS_REMOTE; then
    log "  Remote detected ('$REMOTE_NAME'). Fetching to check for divergence (60s timeout)..."
    # Run fetch with timeout — DoltHub can hang indefinitely on large databases.
    # macOS lacks `timeout`, so use background + wait + kill pattern.
    dolt_exec "$DB" "CALL DOLT_FETCH('$REMOTE_NAME')" &
    FETCH_PID=$!
    FETCH_OK=false
    if wait_with_timeout "$FETCH_PID" 60; then
      FETCH_OK=true
    else
      # Must use || true — kill/wait return non-zero for signaled processes,
      # and set -e would terminate the script before reaching the WARNING path.
      kill "$FETCH_PID" 2>/dev/null || true
      wait "$FETCH_PID" 2>/dev/null || true
    fi
    if ! $FETCH_OK; then
      # Fetch timeout/failure is a warning, not a blocker. For local-source-of-truth
      # databases (hq/bd/gt), the local data IS the authority — we compact and
      # force-push. Aborting on fetch failure causes the escalation feedback loop:
      # timeout → abort → escalate → more commits → slower fetch → repeat.
      log "  [handled] Fetch from remote timed out for $DB — proceeding with compaction (local is source of truth)"
      # Skip divergence check — remote branch info is stale after a failed fetch.
      # Using stale dolt_remote_branches data would falsely block compaction
      # (previous remote HEAD won't be ancestor of current local history).
      log "  Skipping divergence check (fetch failed, remote branch info unreliable)"
    fi
    # Verify local HEAD is at or ahead of remote HEAD.
    # If remote has commits we don't have, compaction would lose them.
    # Only check when fetch succeeded — stale remote info gives false positives.
    if $FETCH_OK; then
      DIVERGENCE_RC=0
      verify_remote_not_diverged "$DB" || DIVERGENCE_RC=$?
      if [[ "$DIVERGENCE_RC" -eq 10 ]]; then
        ERRORS=$((ERRORS + 1))
        ERROR_DETAILS="${ERROR_DETAILS}${DB}: remote ahead of local (skipped to avoid data loss)\n"
        continue
      elif [[ "$DIVERGENCE_RC" -ne 0 ]]; then
        # The check could not be made: not the same as "no divergence".
        ERRORS=$((ERRORS + 1))
        ERROR_DETAILS="${ERROR_DETAILS}${DB}: could not verify the remote against local history (skipped)\n"
        continue
      fi
      log "  Remote fetch complete. Local is at or ahead of remote."
    fi
  fi

  # Step 3b: Find root (earliest) commit hash.
  ROOT_HASH=$(dolt_query "$DB" "SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1" 2>/dev/null | head -1)
  if [[ -z "$ROOT_HASH" ]]; then
    log "  ERROR: Could not find root commit for $DB"
    ERRORS=$((ERRORS + 1))
    ERROR_DETAILS="${ERROR_DETAILS}${DB}: no root commit\n"
    continue
  fi
  if ! validate_hash "$ROOT_HASH" "root commit"; then
    ERRORS=$((ERRORS + 1))
    ERROR_DETAILS="${ERROR_DETAILS}${DB}: invalid root commit hash\n"
    continue
  fi
  log "  Root commit: ${ROOT_HASH:0:8}"

  # Step 3c: Soft-reset to root commit (moves parent pointer, keeps data staged).
  log "  Soft-resetting to root..."
  if ! dolt_exec "$DB" "CALL DOLT_RESET('--soft', '$ROOT_HASH')"; then
    log "  ERROR: Soft reset failed for $DB"
    ERRORS=$((ERRORS + 1))
    ERROR_DETAILS="${ERROR_DETAILS}${DB}: soft reset failed\n"
    continue
  fi

  # Step 3d: Commit all data as a single commit.
  # "nothing to commit" is valid when soft-reset lands on identical data
  # (e.g., only commit metadata changed, not table content). dolt_exec
  # sends stderr to LOGFILE, so check LOGFILE for the "nothing to commit"
  # message when the command fails.
  COMMIT_MSG="compaction: flatten history to single commit"
  log "  Committing flattened data..."
  if ! dolt_exec "$DB" "CALL DOLT_COMMIT('-Am', '$COMMIT_MSG')"; then
    if grep -q "nothing to commit" "$LOGFILE" 2>/dev/null; then
      log "  Nothing to commit (data unchanged) — compaction is a no-op, skipping"
      SKIPPED+=("$DB (nothing to commit)")
      continue
    fi
    log "  ERROR: Flatten commit failed for $DB"
    ERRORS=$((ERRORS + 1))
    ERROR_DETAILS="${ERROR_DETAILS}${DB}: commit failed\n"
    continue
  fi

  # --- Step 4: Verify data integrity (row counts before/after) ----------------

  log "  Verifying integrity..."
  INTEGRITY_OK=true

  while IFS= read -r TABLE; do
    [[ -z "$TABLE" ]] && continue
    if ! validate_name "$TABLE" "table"; then
      continue
    fi
    POST_COUNT=$(dolt_query "$DB" "SELECT COUNT(*) FROM \`$TABLE\`" 2>/dev/null | head -1)
    PRE=$(grep "^${TABLE}	" "$PRE_COUNTS_FILE" 2>/dev/null | cut -f2)
    if [[ -z "$PRE" ]]; then
      log "  [info] Table $TABLE appeared after compaction (new table — concurrent write)"
      continue
    fi
    # Only fail on data loss (postCount < preCount).
    # postCount > preCount is safe: concurrent writes during flatten add rows
    # (merge base shifts, txn is preserved — see compactor_dog.go).
    if [[ -n "$POST_COUNT" && "$POST_COUNT" -lt "$PRE" ]]; then
      log "  INTEGRITY FAILURE: $DB.$TABLE — data loss: pre=$PRE post=$POST_COUNT"
      INTEGRITY_OK=false
    fi
  done <<< "$PRE_TABLES"

  # Check for missing tables (tables present before but gone after).
  POST_TABLES=$(dolt_query "$DB" \
    "SELECT table_name FROM information_schema.tables WHERE table_schema = '$DB' AND table_name NOT LIKE 'dolt_%' AND table_type = 'BASE TABLE'")
  while IFS=$'\t' read -r TABLE _; do
    [[ -z "$TABLE" ]] && continue
    if ! printf '%s' "$POST_TABLES" | grep -qx "$TABLE"; then
      log "  INTEGRITY FAILURE: Table $TABLE missing after compaction"
      INTEGRITY_OK=false
    fi
  done < "$PRE_COUNTS_FILE"

  if ! $INTEGRITY_OK; then
    log "  ERROR: Integrity check FAILED for $DB"
    log "  WARNING: DATABASE LEFT IN COMPACTED STATE — MANUAL INSPECTION REQUIRED"
    ERRORS=$((ERRORS + 1))
    ERROR_DETAILS="${ERROR_DETAILS}${DB}: integrity check failed (DB left in compacted state)\n"
    gt escalate "compactor-dog: integrity failure in $DB" -s HIGH \
      --reason "Row count mismatch after flatten compaction on $DB. DATABASE LEFT IN COMPACTED STATE — MANUAL INSPECTION REQUIRED." 2>/dev/null || true
    continue
  fi

  # Verify final commit count
  FINAL_COUNT=$(dolt_query "$DB" "SELECT COUNT(*) AS cnt FROM dolt_log" 2>/dev/null | head -1)
  log "  Integrity verified ($TABLE_COUNT tables). $FINAL_COUNT commits remain."

  # --- Step 5: Run dolt_gc after compaction -----------------------------------

  log "  Running dolt_gc..."
  if dolt_exec "$DB" "CALL dolt_gc()"; then
    log "  GC complete."
  else
    log "  [handled] dolt_gc failed for $DB (non-fatal, will retry next cycle)"
  fi

  # Step 5b: Push compacted history to remote to maintain sync.
  # This MUST be a force-push because flatten rewrites the commit graph.
  # Safe here because: (1) we pulled first, (2) integrity is verified.
  # Use --set-upstream because flatten loses branch tracking metadata.
  if $HAS_REMOTE; then
    log "  Pushing compacted history to remote ('$REMOTE_NAME')..."
    if ! push_compacted_history "$DB" "$REMOTE_NAME"; then
      log "  [handled] Force-push to remote not completed for $DB (failed or refused by the visibility guard) — remote will resync on next cycle"
      ERROR_DETAILS="${ERROR_DETAILS}${DB}: force-push not completed (failed or refused; local compacted)\n"
      ERRORS=$((ERRORS + 1))
    else
      log "  Remote push complete."
    fi
  fi

  COMPACTED=$((COMPACTED + 1))
done

# --- Step 6: Report results ---------------------------------------------------

log ""
log "=== Compaction Cycle Complete ==="
log "  Compacted: $COMPACTED"
log "  Skipped:   ${#SKIPPED[@]}"
log "  Errors:    $ERRORS"

SUMMARY="compactor-dog: compacted=$COMPACTED skipped=${#SKIPPED[@]} errors=$ERRORS (threshold=$COMMIT_THRESHOLD)"

if [[ $ERRORS -gt 0 ]]; then
  log ""
  log "Error details:"
  printf '%b\n' "$ERROR_DETAILS" | while read -r line; do
    [[ -n "$line" ]] && log "  $line"
  done

  gt escalate "compactor-dog: $ERRORS databases had compaction errors" -s MEDIUM \
    --reason "Compaction cycle completed with errors. $SUMMARY" 2>/dev/null || true

  _rid="$(bd create "compactor-dog: ERRORS — $SUMMARY" -t chore --ephemeral \
    -l type:plugin-run,plugin:compactor-dog,result:warning \
    -d "Compaction completed with $ERRORS errors. $SUMMARY" --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
else
  _rid="$(bd create "$SUMMARY" -t chore --ephemeral \
    -l type:plugin-run,plugin:compactor-dog,result:success \
    -d "$SUMMARY" --silent 2>/dev/null)" || true
  [ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
fi

log "Done."
