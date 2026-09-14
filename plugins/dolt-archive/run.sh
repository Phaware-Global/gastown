#!/usr/bin/env bash
# dolt-archive/run.sh — Deterministic JSONL backup + git push + dolt push.
#
# Exports production databases to JSONL, commits to git backup repo,
# and pushes Dolt remotes. JSONL is the last-resort recovery layer.
#
# Usage: ./run.sh [--databases db1,db2,...] [--skip-git] [--skip-dolt-push]

set -euo pipefail

# --- Configuration -----------------------------------------------------------

DOLT_HOST="${DOLT_HOST:-127.0.0.1}"
DOLT_PORT="${DOLT_PORT:-3307}"
DOLT_USER="${DOLT_USER:-root}"
DOLT_DATA_DIR="${DOLT_DATA_DIR:-$HOME/gt/.dolt-data}"
JSONL_EXPORT_DIR="$HOME/gt/.dolt-archive/jsonl"
BACKUP_REPO="$HOME/gt/.dolt-archive/git"
DEFAULT_DBS="auto"
SKIP_GIT=false
SKIP_DOLT_PUSH=false

# --- Argument parsing --------------------------------------------------------

while [[ $# -gt 0 ]]; do
  case "$1" in
    --databases)    DEFAULT_DBS="$2"; shift 2 ;;
    --skip-git)     SKIP_GIT=true; shift ;;
    --skip-dolt-push) SKIP_DOLT_PUSH=true; shift ;;
    --help|-h)
      echo "Usage: $0 [--databases db1,db2,...] [--skip-git] [--skip-dolt-push]"
      exit 0
      ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# --- Helpers -----------------------------------------------------------------

log() {
  echo "[dolt-archive] $*"
}

# Indent borrowed multi-line output so it cannot forge its own [dolt-archive] line.
# Normalize lone CR bytes to newlines first — sed's ^ only anchors after \n, so
# CR-delimited content (spinner output, or a remote injecting a bare CR) would
# otherwise ride through as one unprefixed "line".
logblock() {
  printf '%s\n' "$1" | tr '\r' '\n' | sed 's/^/[dolt-archive]     | /'
}

# Strip userinfo (user:token@) from URLs so credentialed remotes never hit the log.
redact() {
  sed -E 's#(://)[^/@[:space:]]*@#\1***@#g'
}

dolt_query() {
  local db="$1"
  local query="$2"
  local args=(dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --no-tls -u "$DOLT_USER" -p "")
  if [[ -n "$db" ]]; then
    args+=(--use-db "$db")
  fi
  args+=(sql -q "$query" --result-format csv)
  "${args[@]}" | tail -n +2 | tr -d '\r'
}

dolt_query_json() {
  local db="$1"
  local query="$2"
  dolt --host "$DOLT_HOST" --port "$DOLT_PORT" --no-tls -u "$DOLT_USER" -p "" \
    --use-db "$db" sql -q "$query" --result-format json
}

# --- Step 1: JSONL export ----------------------------------------------------

# Auto-discover production databases or use the explicit list.
if [[ "$DEFAULT_DBS" == "auto" ]]; then
  PROD_DBS=()
  while IFS= read -r line; do
    PROD_DBS+=("$line")
  done < <(
    dolt_query "" "SHOW DATABASES" \
      | grep -v -E '^(information_schema|mysql|dolt_cluster)$' \
      | grep -v -E '^(testdb_|beads_t|beads_pt|doctest_)'
  )
  if [[ ${#PROD_DBS[@]} -eq 0 ]]; then
    log "ERROR: No production databases found via auto-discovery"
    exit 1
  fi
else
  IFS=',' read -ra PROD_DBS <<< "$DEFAULT_DBS"
fi

log "Starting archive cycle (databases: ${PROD_DBS[*]})"
mkdir -p "$JSONL_EXPORT_DIR"

EXPORTED=0
EXPORT_FAILED=0
EXPORT_ERRORS=""

for DB in "${PROD_DBS[@]}"; do
  EXPORT_FILE="$JSONL_EXPORT_DIR/${DB}-$(date +%Y%m%d-%H%M).jsonl"
  LATEST_LINK="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"

  log "Exporting $DB..."

  # A query failure (can't connect, auth, etc.) is not the same as a
  # genuinely empty result — the former is an export failure, the latter
  # just means this DB has no issues table (e.g. gastown config DB).
  QERR=$(mktemp)
  if ! TABLE_CHECK=$(dolt_query "$DB" "SHOW TABLES LIKE 'issues'" 2>"$QERR"); then
    CAUSE=$(tr '\n' ' ' < "$QERR"); rm -f "$QERR"
    log "  WARN: $DB: table check query failed: $CAUSE"
    EXPORT_FAILED=$((EXPORT_FAILED + 1))
    EXPORT_ERRORS="${EXPORT_ERRORS}${DB}(table-check: $CAUSE) "
    continue
  fi
  rm -f "$QERR"
  if ! grep -q 'issues' <<< "$TABLE_CHECK"; then
    log "  $DB: skipped (no issues table)"
    continue
  fi

  # Export via Dolt SQL (reliable for all databases with an issues table)
  QERR=$(mktemp)
  if dolt_query_json "$DB" "SELECT * FROM issues ORDER BY id" > "$EXPORT_FILE" 2>"$QERR" && [[ -s "$EXPORT_FILE" ]]; then
    LINE_COUNT=$(wc -l < "$EXPORT_FILE" | tr -d ' ')
    log "  $DB: exported via SQL ($LINE_COUNT lines)"
    ln -sf "$(basename "$EXPORT_FILE")" "$LATEST_LINK"
    EXPORTED=$((EXPORTED + 1))
    rm -f "$QERR"
  else
    CAUSE=$(tr '\n' ' ' < "$QERR"); rm -f "$QERR"
    log "  WARN: $DB export failed: $CAUSE"
    rm -f "$EXPORT_FILE"
    EXPORT_FAILED=$((EXPORT_FAILED + 1))
    EXPORT_ERRORS="${EXPORT_ERRORS}${DB}(export: $CAUSE) "
  fi
done

# Prune old exports (keep last 24 snapshots per DB)
for DB in "${PROD_DBS[@]}"; do
  SNAPSHOTS=$(ls -t "$JSONL_EXPORT_DIR/${DB}-2"*.jsonl 2>/dev/null | tail -n +25)
  if [[ -n "$SNAPSHOTS" ]]; then
    echo "$SNAPSHOTS" | xargs rm -f
    log "Pruned old $DB snapshots"
  fi
done

log "JSONL export: $EXPORTED succeeded, $EXPORT_FAILED failed"

# --- Step 2: Git commit and push ---------------------------------------------

GIT_PUSHED=false
GIT_FAILED=false
GIT_FAIL_STAGE=""
GIT_SKIP_REASON=""
GIT_UP_TO_DATE=false
GIT_NO_UPSTREAM_REF=false

if ! $SKIP_GIT && [[ -d "$BACKUP_REPO/.git" ]]; then
  log ""
  log "=== Git Push ==="

  # Copy latest JSONL files to git repo
  for DB in "${PROD_DBS[@]}"; do
    LATEST="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"
    if [[ -L "$LATEST" ]]; then
      REAL_FILE="$JSONL_EXPORT_DIR/$(readlink "$LATEST")"
      if [[ -f "$REAL_FILE" ]]; then
        cp "$REAL_FILE" "$BACKUP_REPO/${DB}.jsonl"
      fi
    elif [[ -f "$LATEST" ]]; then
      cp "$LATEST" "$BACKUP_REPO/${DB}.jsonl"
    fi
  done

  cd "$BACKUP_REPO"

  if git diff --quiet && git diff --staged --quiet; then
    log "No changes to commit"
  else
    if ! ADD_ERR=$(git add *.jsonl 2>&1); then
      log "WARN: git add failed:"
      logblock "$ADD_ERR"
      GIT_FAILED=true
      GIT_FAIL_STAGE="git add"
    fi

    if ! COMMIT_ERR=$(git commit -m "Archive snapshot $(date +%Y-%m-%d-%H%M)" \
      --author="Gas Town Archive <archive@gastown.local>" 2>&1); then
      if [[ "$COMMIT_ERR" == *"nothing to commit"* ]]; then
        # Not a failure — the pre-check above only guarantees a repo-wide diff
        # exists, not that *.jsonl itself changed (e.g. unchanged export content).
        log "No changes staged to commit"
      else
        log "WARN: git commit failed:"
        logblock "$COMMIT_ERR"
        GIT_FAILED=true
        GIT_FAIL_STAGE="git commit"
      fi
    fi

  fi

  # Push whenever local HEAD is ahead of origin/main — not just when this
  # cycle committed something. A commit stranded by an earlier failed push
  # (e.g. a network blip) must still be retried on a later cycle, even one
  # that stages nothing new itself.
  #
  # Remote presence is checked first, independent of rev-list state: a repo
  # with no remote and nothing ahead of origin/main must still report "no
  # remote configured" rather than silently falling through with no reason
  # at all.
  if git remote get-url origin > /dev/null 2>&1; then
    if ! git rev-parse --verify origin/main > /dev/null 2>&1; then
      # origin/main is unknown to this local clone. This is NOT the same as
      # "nothing to push" — `git rev-list origin/main..HEAD` would fail
      # silently on a missing ref and read as an empty (up-to-date) diff.
      # A repo that has never established this ref has never confirmed an
      # offsite copy, so always attempt the push rather than falling through.
      GIT_NO_UPSTREAM_REF=true
      log "WARN: origin/main ref unknown to local clone — cannot confirm any prior offsite copy"
    fi

    if $GIT_NO_UPSTREAM_REF || [[ -n "$(git rev-list origin/main..HEAD 2>/dev/null)" ]]; then
      if PUSH_ERR=$(git push origin main 2>&1); then
        GIT_PUSHED=true
        log "Pushed to GitHub"
      else
        log "WARN: Git push to remote failed:"
        logblock "$(printf '%s' "$PUSH_ERR" | redact)"
        GIT_FAILED=true
        GIT_FAIL_STAGE="git push"
      fi
    else
      # Nothing ahead of origin/main — the offsite copy is already current,
      # not missing. Distinct from GIT_SKIP_REASON/GIT_FAILED so the report
      # step doesn't mistake "up to date" for "no offsite copy exists".
      GIT_UP_TO_DATE=true
      log "Backup repo already up to date with origin/main"
    fi
  else
    log "WARN: No git remote configured for backup repo"
    GIT_SKIP_REASON="no git remote configured for \$BACKUP_REPO"
  fi
elif ! $SKIP_GIT; then
  log "No git backup repo at $BACKUP_REPO — skipping git push"
  GIT_SKIP_REASON="no .git at \$BACKUP_REPO"
else
  GIT_SKIP_REASON="git push skipped (--skip-git)"
fi

# --- Step 3: Dolt native push ------------------------------------------------

# Per-database name lists, not counts. A count that mixes units (e.g. pushes
# vs. databases) can silently read as coverage it doesn't have; a name list
# cannot be the wrong unit. Each database lands in exactly one bucket.
DOLT_PUSHED_LIST=""
DOLT_PUSH_FAILED_LIST=""
DOLT_NO_REMOTE_LIST=""
DOLT_NO_DOLTDIR_LIST=""
DOLT_SKIP_REASON=""
ALL_PROTECTED=false

append_csv() {
  if [[ -z "$1" ]]; then
    echo "$2"
  else
    echo "$1,$2"
  fi
}

if ! $SKIP_DOLT_PUSH; then
  log ""
  log "=== Dolt Push ==="
  ALL_PROTECTED=true

  for DB in "${PROD_DBS[@]}"; do
    DB_DIR="$DOLT_DATA_DIR/$DB"

    if [[ ! -d "$DB_DIR/.dolt" ]]; then
      log "  $DB: no .dolt directory, skipping"
      DOLT_NO_DOLTDIR_LIST=$(append_csv "$DOLT_NO_DOLTDIR_LIST" "$DB")
      ALL_PROTECTED=false
      continue
    fi

    REMOTES=$(cd "$DB_DIR" && { dolt remote -v 2>/dev/null | grep -v "^$" | head -5 || true; })
    if [[ -z "$REMOTES" ]]; then
      log "  $DB: no remotes configured, skipping"
      DOLT_NO_REMOTE_LIST=$(append_csv "$DOLT_NO_REMOTE_LIST" "$DB")
      ALL_PROTECTED=false
      continue
    fi

    log "  $DB: pushing to remotes..."
    cd "$DB_DIR"

    DB_PUSHED=false
    for REMOTE_NAME in $(dolt remote -v 2>/dev/null | awk '{print $1}' | sort -u || true); do
      if PUSH_ERR=$(timeout 120 dolt push "$REMOTE_NAME" main 2>&1); then
        log "    $REMOTE_NAME: pushed"
        DB_PUSHED=true
      else
        log "    $REMOTE_NAME: FAILED:"
        logblock "$(printf '%s' "$PUSH_ERR" | redact)"
      fi
    done

    # A database counts as protected once ANY of its remotes confirms a
    # push — a database with several remotes contributes at most one name
    # to one list, never one name per remote.
    if $DB_PUSHED; then
      DOLT_PUSHED_LIST=$(append_csv "$DOLT_PUSHED_LIST" "$DB")
    else
      DOLT_PUSH_FAILED_LIST=$(append_csv "$DOLT_PUSH_FAILED_LIST" "$DB")
      ALL_PROTECTED=false
    fi
  done

  log "Dolt push: dolt_pushed=[$DOLT_PUSHED_LIST] dolt_push_failed=[$DOLT_PUSH_FAILED_LIST]"
else
  DOLT_SKIP_REASON="dolt push skipped (--skip-dolt-push)"
fi

# --- Step 4: Report results --------------------------------------------------

log ""
log "=== Archive Cycle Complete ==="

# Report what happened; judge nothing. Earlier rounds tried to answer "is
# this the KNOWN config gap or a NEW failure?" — a stateless script has no
# memory of yesterday's run, so every attempt to classify absence vs.
# failure has an edge that reads the wrong way (a broken probe reads as "no
# remote configured"; one database pushing reads as "everything is fine").
#
# No N-of-M count. A count needs a numerator and a denominator in the same
# unit, and every attempt so far has let them drift (pushes vs. databases).
# RESULT is a plain boolean over the set: success only if every database in
# PROD_DBS landed in dolt_pushed. The *_LIST variables name exactly which
# databases are exposed, which a count can't do and can't get wrong.
DBS_TOTAL=${#PROD_DBS[@]}

if $ALL_PROTECTED; then
  RESULT="success"
else
  RESULT="failure"
fi

# git backup (JSONL export + push) is the last-resort recovery layer (see
# header), not the primary offsite copy — reported as its own fact here.
if $GIT_PUSHED; then
  if $GIT_NO_UPSTREAM_REF; then
    GIT_REASON="pushed (origin/main ref was previously unknown)"
  else
    GIT_REASON="pushed"
  fi
elif $GIT_UP_TO_DATE; then
  GIT_REASON="already up to date"
elif $GIT_FAILED; then
  if $GIT_NO_UPSTREAM_REF; then
    GIT_REASON="${GIT_FAIL_STAGE:-git push} failed (origin/main ref unknown — never confirmed pushed)"
  else
    GIT_REASON="${GIT_FAIL_STAGE:-git push} failed"
  fi
elif [[ -n "$GIT_SKIP_REASON" ]]; then
  GIT_REASON="$GIT_SKIP_REASON"
else
  GIT_REASON="unknown"
fi

SUMMARY="Archive: jsonl=$EXPORTED/$((EXPORTED + EXPORT_FAILED)), dbs=$DBS_TOTAL dolt_pushed=$DOLT_PUSHED_LIST dolt_no_remote=$DOLT_NO_REMOTE_LIST dolt_no_doltdir=$DOLT_NO_DOLTDIR_LIST dolt_push_failed=$DOLT_PUSH_FAILED_LIST, git_pushed=$GIT_PUSHED git_reason=\"$GIT_REASON\", result=$RESULT"
log "$SUMMARY"

_rid="$(bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:dolt-archive,result:$RESULT \
  -d "$SUMMARY" --silent 2>/dev/null)" || true
[ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true

if [[ "$EXPORT_FAILED" -gt 0 ]]; then
  if ! ESCALATE_ERR=$(gt escalate "dolt-archive: JSONL export failed for $EXPORT_FAILED databases ($EXPORT_ERRORS)" \
    -s critical \
    --reason "JSONL is our last-resort recovery layer. Failed databases: $EXPORT_ERRORS" 2>&1); then
    log "WARN: gt escalate failed:"
    logblock "$ESCALATE_ERR"
  fi
fi

log "Done."
