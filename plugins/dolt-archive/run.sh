#!/usr/bin/env bash
# dolt-archive/run.sh — Deterministic JSONL backup + git push + dolt push.
#
# Exports production databases to JSONL, commits to git backup repo,
# and pushes Dolt remotes. JSONL is the last-resort recovery layer. Both push
# layers refuse any destination not confirmed private (visibility_guard.sh).
#
# Usage: ./run.sh [--databases db1,db2,...] [--skip-git] [--skip-dolt-push]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./visibility_guard.sh
source "$SCRIPT_DIR/visibility_guard.sh"

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

# Escalation dedupe state (gt-4kip). `gt escalate --fingerprint` only suppresses
# against OPEN beads, so a closed repeat is re-filed on the next cycle. This
# remembers what was already escalated so an unchanged condition stays quiet.
STATE_DIR="${DOLT_ARCHIVE_STATE_DIR:-$HOME/gt/.dolt-archive/escalation-state}"
# Re-notify (at low severity) for a condition that stays unchanged this long.
REPEAT_SECS="${ESCALATION_REPEAT_SECS:-86400}"

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

# A <db>-latest.jsonl left by an earlier cycle is not this cycle's snapshot.
# Step 2 copies whatever it points at into the git backup repo and commits it
# as "Archive snapshot <now>", so once this cycle's export has failed it must
# not survive — otherwise old data is committed under today's name and the run
# reads as healthy. Dated snapshot files are history, not a claim of currency,
# and are left alone.
#
# Removal is best-effort: run.sh runs under set -e, so a bare failing rm would
# abort the cycle before the summary, receipt and escalation. If the link can't
# be removed (e.g. read-only jsonl dir) the db is recorded in STALE_LATEST and
# Step 2 refuses to copy it, so the guard still fails closed.
STALE_LATEST=()
discard_stale_latest() {
  local db="$1"
  local link="$JSONL_EXPORT_DIR/${db}-latest.jsonl"
  if [[ -e "$link" || -L "$link" ]]; then
    if rm -f "$link" 2>/dev/null; then
      log "  WARN: $db removed stale ${db}-latest.jsonl (export failed; it would have republished an older snapshot as current)"
    else
      STALE_LATEST+=("$db")
      log "  WARN: $db could not remove stale ${db}-latest.jsonl (export failed); it will not be copied to the git backup"
    fi
  fi
}

# --- Escalation dedupe (gt-4kip) ---------------------------------------------
#
# Every condition has a KEY (which condition) and a SIG (its affected set: which
# DBs/remotes, which URL+visibility). State per key is "<sig-hash> <epoch> <key> <covered>".
#   - no state, or SIG changed  -> escalate critical (new, or materially different)
#   - same SIG, < REPEAT_SECS   -> log only, no escalation
#   - same SIG, >= REPEAT_SECS  -> escalate once more at LOW (digest)
#   - condition absent this run -> state dropped, so its return escalates afresh
# State is written only after `gt escalate` succeeds, so a failed escalation is
# retried on the next cycle rather than silently swallowed. SIGs are stored only
# as a hash: they may embed a remote URL, which must not land on disk in the clear.

ACTIVE_KEYS=()

state_file() {
  local safe="${1//[^A-Za-z0-9_.-]/_}"
  printf '%s/%s-%s' "$STATE_DIR" "$safe" "$(printf '%s' "$1" | cksum | cut -d' ' -f1)"
}

key_is_active() {
  local k
  for k in "${ACTIVE_KEYS[@]:-}"; do
    [[ "$k" == "$1" ]] && return 0
  done
  return 1
}

# Deterministic, order-insensitive rendering of a whitespace-separated set.
sorted_set() {
  # An all-empty input (every arg "") is a legitimate empty set, not an
  # error: grep -v '^$' then filters out every line and exits 1, which
  # would otherwise abort the whole script under set -e -o pipefail at
  # any call site that assigns this into a variable (e.g. the covered-set
  # union in escalate_once, whose args are "" until a condition has ever
  # recorded one).
  printf '%s\n' "$@" | tr ' ' '\n' | grep -v '^$' | sort -u | tr '\n' ' '
  return 0
}

# member_hashes SIG — sorted, comma-joined (trailing comma), per-token cksum
# hash of a whitespace-separated set. Hashing per member (instead of the
# whole string at once) lets escalate_once tell "a member disappeared"
# (shrink, e.g. partial recovery) from "a member appeared" (growth) instead
# of treating any change to the set as equally new — a flapping member
# otherwise re-escalates at critical on every toggle (adversarial finding
# on #250). Members are hashed, not stored in the clear, because some
# callers' sigs embed a remote URL.
member_hashes() {
  local -a items
  local item out=""
  read -ra items <<< "$1"
  for item in "${items[@]}"; do
    [[ -n "$item" ]] || continue
    out="${out}$(printf '%s' "$item" | cksum | cut -d' ' -f1)"$'\n'
  done
  printf '%s' "$out" | sort -u | tr '\n' ','
}

# hash_set_is_subset A B — true if every hash in comma-joined set A (as
# produced by member_hashes) is also present in comma-joined set B.
hash_set_is_subset() {
  local hay=",$2" item
  local -a items
  IFS=',' read -ra items <<< "$1"
  for item in "${items[@]}"; do
    [[ -n "$item" ]] || continue
    [[ "$hay" == *",$item,"* ]] || return 1
  done
  return 0
}

# escalate_once KEY SIG TITLE REASON [COVERED_DBS]
#
# COVERED_DBS (optional) is the whitespace-separated set of databases this
# condition concerns. It is recorded so clear_resolved can tell "not looked
# at this run" apart from "looked at and no longer failing" on a
# --databases subset run (adversarial finding on #250). Omitted for
# conditions with no db-scoped meaning (push-refused's db already lives in
# its key; the git-backup keys concern no db at all).
#
# Whenever cur_hashes carries forward any previously-recorded members (the
# digest-repeat path, or the growth-union path below), the recorded COVERED
# set must carry the same union — not just this run's covered arg. Val's
# HIGH on #250: the growth-union kept hashes for old+new members but recorded
# 'covered' for the new members only, so a later --databases subset run (that
# only checks the new member) saw the old member's db as "out of scope" and
# cleared its state, letting a still-unresolved condition re-file as if new.
escalate_once() {
  local key="$1" sig="$2" title="$3" reason="$4" covered="${5:-}"
  local now file prev_hashes="" last="" _k prev_covered="" severity="critical" fp cur_hashes

  ACTIVE_KEYS+=("$key")
  now="$(date +%s)"
  file="$(state_file "$key")"
  cur_hashes="$(member_hashes "$sig")"
  fp="dolt-archive:${key}:$(printf '%s' "$cur_hashes" | cksum | cut -d' ' -f1)"

  if [[ -f "$file" ]]; then
    read -r prev_hashes last _k prev_covered < "$file" || true
    # Unreadable state or a clock that went backwards: treat as no state.
    if [[ -n "$prev_hashes" && "$last" =~ ^[0-9]+$ && "$last" -le "$now" ]]; then
      if [[ "$cur_hashes" == "$prev_hashes" ]] || hash_set_is_subset "$cur_hashes" "$prev_hashes"; then
        # Same set, or every currently-affected member already escalated as
        # part of a larger set (a pure shrink/partial recovery). Either way,
        # stay quiet until REPEAT_SECS, then send one low digest and keep
        # recording the larger set so a member flapping back into it doesn't
        # read as "new" (adversarial finding on #250).
        if (( now - last < REPEAT_SECS )); then
          if [[ "$cur_hashes" == "$prev_hashes" ]]; then
            log "  $key: unchanged, already escalated $(( (now - last) / 60 ))m ago — not re-escalating"
          else
            log "  $key: affected set shrank (partial recovery) — not re-escalating"
          fi
          return 0
        fi
        severity="low"
        title="$title (still unresolved)"
        fp="$fp:digest-$(date +%Y%m%d)"
        cur_hashes="$prev_hashes"
        covered="$(sorted_set "$covered" "$prev_covered")"
      else
        # The current set has a member the recorded set lacks (growth, or a
        # simultaneous grow+shrink) — escalate fresh, but record the union
        # so an already-escalated member isn't forgotten and re-escalated
        # as "new" on a later flap (adversarial finding on #250). The
        # covered set must grow with it (val's HIGH on #250) so a later
        # subset run that only checks the new member does not read the old
        # member's db as never-checked and clear its still-unresolved state.
        cur_hashes="$(printf '%s' "${cur_hashes}${prev_hashes}" | tr ',' '\n' | sed '/^$/d' | sort -u | tr '\n' ',')"
        covered="$(sorted_set "$covered" "$prev_covered")"
      fi
    fi
  fi

  if ! ESCALATE_ERR=$(gt escalate "$title" -s "$severity" --fingerprint "$fp" --reason "$reason" 2>&1); then
    log "WARN: gt escalate failed (will retry next cycle):"
    logblock "$ESCALATE_ERR"
    return 0
  fi
  mkdir -p "$STATE_DIR" 2>/dev/null || true
  printf '%s %s %s %s\n' "$cur_hashes" "$now" "$key" "$covered" > "$file" 2>/dev/null \
    || log "WARN: cannot record escalation state in $STATE_DIR — repeats will re-escalate"
}

# Drop state for conditions that did not fire this cycle. Called only for the
# steps that actually ran, so --skip-* never reads as "condition cleared".
# $1 is a key or, with a trailing '*', a key prefix. $2, when given, is this
# run's checked-db set (space-separated): a key concerning a db outside it
# was not looked at this cycle, so its absence from ACTIVE_KEYS means
# "out of scope", not "resolved" — a --databases subset run must not clear
# another db's state (adversarial finding on #250). The guard only applies
# when --databases was explicitly given: on an auto-discovery run, PROD_DBS
# is every live database, so a covered db that no longer exists (dropped or
# renamed) was never "checked" and would otherwise pin its state forever —
# it counts as resolved instead (adversarial finding on #250).
clear_resolved() {
  local pattern="$1" checked_dbs="${2:-}" f k covered required db item ok
  [[ -d "$STATE_DIR" ]] || return 0
  for f in "$STATE_DIR"/*; do
    [[ -f "$f" ]] || continue
    k="" covered=""
    read -r _ _ k covered < "$f" || true
    [[ -n "$k" ]] || continue
    case "$pattern" in
      *'*') [[ "$k" == "${pattern%\*}"* ]] || continue ;;
      *)    [[ "$k" == "$pattern" ]] || continue ;;
    esac
    key_is_active "$k" && continue

    if [[ -n "$checked_dbs" && "$DEFAULT_DBS" != "auto" ]]; then
      required="$covered"
      if [[ -z "$required" && "$pattern" == *':*' ]]; then
        # Per-db key (e.g. push-refused:<db>:<remote>): the db is the
        # segment between the key's first two colons.
        db="${k#*:}"; required="${db%%:*}"
      fi
      if [[ -n "$required" ]]; then
        ok=true
        for item in ${required//,/ }; do
          case " $checked_dbs " in
            *" $item "*) ;;
            *) ok=false; break ;;
          esac
        done
        $ok || continue
      fi
    fi

    rm -f "$f"
  done
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
EXPORT_FAILED_DBS=""
EXPORTED_DBS=()

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
    discard_stale_latest "$DB"
    EXPORT_FAILED=$((EXPORT_FAILED + 1))
    EXPORT_ERRORS="${EXPORT_ERRORS}${DB}(table-check: $CAUSE) "
    EXPORT_FAILED_DBS="${EXPORT_FAILED_DBS}${DB} "
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
    EXPORTED_DBS+=("$DB")
    rm -f "$QERR"
  else
    CAUSE=$(tr '\n' ' ' < "$QERR"); rm -f "$QERR"
    log "  WARN: $DB export failed: $CAUSE"
    rm -f "$EXPORT_FILE"
    discard_stale_latest "$DB"
    EXPORT_FAILED=$((EXPORT_FAILED + 1))
    EXPORT_ERRORS="${EXPORT_ERRORS}${DB}(export: $CAUSE) "
    EXPORT_FAILED_DBS="${EXPORT_FAILED_DBS}${DB} "
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
GIT_REPO_MISSING=false
GIT_PUSH_REFUSED=0
GIT_REFUSAL_DETAIL=""
GIT_GUARD_RAN=false

if ! $SKIP_GIT && [[ -d "$BACKUP_REPO/.git" ]]; then
  log ""
  log "=== Git Push ==="

  # Copy latest JSONL files to git repo
  for DB in "${PROD_DBS[@]}"; do
    LATEST="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"
    _stale=false
    for _stale_db in "${STALE_LATEST[@]:-}"; do
      [[ "$_stale_db" == "$DB" ]] && _stale=true
    done
    if $_stale; then
      log "  $DB: skipping copy of stale ${DB}-latest.jsonl (export failed and the link could not be removed)"
      continue
    fi
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
      fi
    fi

  fi

  # Push whenever local HEAD is ahead of origin/main — not just when this
  # cycle committed something. A commit stranded by an earlier failed push
  # (e.g. a network blip) must still be retried on a later cycle, even one
  # that stages nothing new itself.
  #
  # `git rev-list origin/main..HEAD` exits non-zero with EMPTY stdout when
  # origin isn't configured, or refs/remotes/origin/main doesn't exist —
  # indistinguishable, once stderr is discarded, from a genuine "nothing
  # ahead". Check its own exit status instead of just testing the (possibly
  # error-empty) output, so a missing push destination reads as a failure
  # rather than the misleading all-clear.
  if AHEAD="$(git rev-list origin/main..HEAD 2>&1)"; then
    if [[ -n "$AHEAD" ]]; then
      # Same fail-closed gate as the dolt push below: refuse unless every
      # push URL is a GitHub repo confirmed private. The JSONL is an export
      # of the production issue/mail databases — see gt-sg6n. Refusal is not
      # a push failure (the guard working as intended), so it is counted and
      # escalated on its own, never folded into GIT_FAILED.
      GIT_GUARD_RAN=true
      if VIS_REASON=$(git_push_allowed origin); then
        if PUSH_ERR=$(git push origin main 2>&1); then
          GIT_PUSHED=true
          log "Pushed to GitHub"
        else
          log "WARN: Git push to remote failed:"
          logblock "$(printf '%s' "$PUSH_ERR" | redact)"
          GIT_FAILED=true
        fi
      elif [[ "$VIS_REASON" == "push-url-unresolvable" ]]; then
        # No push destination at all (origin removed but a stale
        # refs/remotes/origin/main survives) — nothing was judged unsafe,
        # the push simply cannot happen. Same failure as a missing origin.
        log "WARN: Cannot resolve a push URL for origin (remote removed, stale refs/remotes/origin/main?):"
        GIT_FAILED=true
      else
        PUSH_URLS="$(git remote get-url --push --all origin 2>/dev/null | tr '\n' ' ' | redact || true)"
        log "WARN: git origin: REFUSED — visibility=$VIS_REASON (not confirmed private): ${PUSH_URLS:-<unresolvable>}"
        GIT_PUSH_REFUSED=$((GIT_PUSH_REFUSED + 1))
        GIT_REFUSAL_DETAIL="origin(${VIS_REASON})"
      fi
    fi
  else
    log "WARN: Cannot determine git push status (no origin remote, or missing refs/remotes/origin/main):"
    logblock "$(printf '%s' "$AHEAD" | redact)"
    GIT_FAILED=true
  fi
elif ! $SKIP_GIT; then
  log "No git backup repo at $BACKUP_REPO — skipping git push"
  GIT_REPO_MISSING=true
fi

# --- Step 3: Dolt native push ------------------------------------------------

DOLT_PUSHED=0
DOLT_PUSH_FAILED=0
DBS_WITH_REMOTE=0
EXPORTED_DBS_WITH_REMOTE=0
DOLT_PUSH_REFUSED=0
DOLT_REFUSAL_DETAIL=""
DOLT_PUSH_FAILED_SET=""
DBS_WITH_REMOTE_SET=""

if ! $SKIP_DOLT_PUSH; then
  log ""
  log "=== Dolt Push ==="

  for DB in "${PROD_DBS[@]}"; do
    DB_DIR="$DOLT_DATA_DIR/$DB"

    if [[ ! -d "$DB_DIR/.dolt" ]]; then
      log "  $DB: no .dolt directory, skipping"
      continue
    fi

    REMOTES=$(cd "$DB_DIR" && { dolt remote -v 2>/dev/null | grep -v "^$" || true; })
    if [[ -z "$REMOTES" ]]; then
      log "  $DB: no remotes configured, skipping"
      continue
    fi

    DBS_WITH_REMOTE=$((DBS_WITH_REMOTE + 1))
    DBS_WITH_REMOTE_SET="${DBS_WITH_REMOTE_SET}${DB} "
    # Population for the shortfall check below must match EXPORTED (databases
    # actually exported), not all databases with a remote — a never-exported
    # db with a remote must not mask an exported db that lacks one.
    for _exported_db in "${EXPORTED_DBS[@]:-}"; do
      if [[ "$_exported_db" == "$DB" ]]; then
        EXPORTED_DBS_WITH_REMOTE=$((EXPORTED_DBS_WITH_REMOTE + 1))
        break
      fi
    done
    log "  $DB: pushing to remotes..."
    cd "$DB_DIR"

    while IFS=$' \t' read -r REMOTE_NAME REMOTE_URL _; do
      [[ -z "$REMOTE_NAME" ]] && continue

      # Refuse unless the remote is a GitHub repo confirmed private. Fails
      # closed: public, non-GitHub, or an undeterminable visibility all
      # refuse the same way. This is the only thing standing between this
      # script and a push to a public repo — see gt-v3df.
      if VIS_REASON=$(remote_push_allowed "$REMOTE_URL"); then
        if PUSH_ERR=$(timeout 120 dolt push "$REMOTE_NAME" main 2>&1); then
          log "    $REMOTE_NAME: pushed"
          DOLT_PUSHED=$((DOLT_PUSHED + 1))
        else
          log "    $REMOTE_NAME: FAILED:"
          logblock "$(printf '%s' "$PUSH_ERR" | redact)"
          DOLT_PUSH_FAILED=$((DOLT_PUSH_FAILED + 1))
          DOLT_PUSH_FAILED_SET="${DOLT_PUSH_FAILED_SET}${DB}/${REMOTE_NAME} "
        fi
      else
        log "    $REMOTE_NAME: REFUSED — visibility=$VIS_REASON (not confirmed private): $(printf '%s' "$REMOTE_URL" | redact)"
        DOLT_PUSH_REFUSED=$((DOLT_PUSH_REFUSED + 1))
        DOLT_REFUSAL_DETAIL="${DOLT_REFUSAL_DETAIL}${DB}/${REMOTE_NAME}(${VIS_REASON}) "

        escalate_once "push-refused:${DB}:${REMOTE_NAME}" "${DB}|${REMOTE_NAME}|${REMOTE_URL}|${VIS_REASON}" \
          "dolt-archive: refused dolt push for $DB to unsafe remote $REMOTE_NAME" \
          "Remote visibility is '$VIS_REASON', not confirmed private. Refusing to push $DB to $(printf '%s' "$REMOTE_URL" | redact) until it is. See gt-v3df."
      fi
    done <<< "$REMOTES"
  done

  log "Dolt push: $DOLT_PUSHED succeeded, $DOLT_PUSH_FAILED failed, $DOLT_PUSH_REFUSED refused (unsafe destination)"
fi

# --- Step 4: Report results --------------------------------------------------

log ""
log "=== Archive Cycle Complete ==="

# A remote-configured count (among EXPORTED databases specifically) below
# the exported count means some exported databases were never even
# attempted by dolt push — they can't show up in DOLT_PUSH_FAILED because
# they were never tried. Compared against EXPORTED_DBS_WITH_REMOTE, not the
# raw DBS_WITH_REMOTE total, since that total also counts databases that
# were never exported and would otherwise mask a shortfall among the ones
# that were. Only meaningful when dolt push actually ran this cycle.
REMOTE_SHORTFALL=false
if ! $SKIP_DOLT_PUSH && [[ "$EXPORTED_DBS_WITH_REMOTE" -lt "$EXPORTED" ]]; then
  REMOTE_SHORTFALL=true
fi

RESULT="success"
if [[ "$EXPORT_FAILED" -gt 0 ]] || [[ "$DOLT_PUSH_FAILED" -gt 0 ]] || [[ "$DOLT_PUSH_REFUSED" -gt 0 ]] || [[ "$GIT_PUSH_REFUSED" -gt 0 ]] || $GIT_FAILED || $GIT_REPO_MISSING || $REMOTE_SHORTFALL; then
  RESULT="warning"
fi

# dolt_push's own ratio (DOLT_PUSHED/DOLT_PUSH_FAILED/DOLT_PUSH_REFUSED) is
# attempted across DBS_WITH_REMOTE — every PROD_DBS entry with a remote
# configured, exported or not. A refusal is not the same as "nothing to
# push" — it must show up in the denominator as an attempted-but-blocked
# push, not vanish into the count of things that just weren't attempted —
# but it is also not the same as a FAILED push (the guard working as
# intended), so it gets its own explicit token rather than folding into
# DOLT_PUSH_FAILED. EXPORTED_DBS_WITH_REMOTE/EXPORTED is a separate
# population (remotes among exported databases only, for the shortfall check
# above). These figures must not be joined with "of" — that reads as one
# ratio nested inside the other's denominator, when they're independently
# counted and can disagree (e.g. a push succeeding on an unexported db while
# every exported db lacks a remote). Report them side by side instead, each
# self-contained. Both figures come from the Dolt Push loop, which never
# runs under --skip-dolt-push — reporting them as 0 there would read as
# "checked, found none" rather than "not checked", so state that the step
# was skipped instead.
if $SKIP_DOLT_PUSH; then
  DOLT_PUSH_CLAUSE="dolt_push=skipped"
else
  DOLT_PUSH_CLAUSE="dolt_push=$DOLT_PUSHED/$((DOLT_PUSHED + DOLT_PUSH_FAILED + DOLT_PUSH_REFUSED)) attempted across $DBS_WITH_REMOTE db(s) with a remote, dolt_push_refused=$DOLT_PUSH_REFUSED, exported_dbs_with_remote=$EXPORTED_DBS_WITH_REMOTE/$EXPORTED"
fi

# git=true/false collapsed four different states into one token: skipped
# (--skip-git), missing (no backup repo at $BACKUP_REPO), failed (add/commit/
# push error), and nothing-to-push (repo present, nothing ahead of
# origin/main) all read as "false". Same vocabulary as dolt_push's own
# skipped/attempted distinction above. Checked in priority order: a push that
# actually succeeded wins even if an earlier step in the same cycle failed; a
# visibility refusal outranks a plain failure so it is never read as one.
if $SKIP_GIT; then
  GIT_CLAUSE="git=skipped"
elif $GIT_REPO_MISSING; then
  GIT_CLAUSE="git=missing"
elif $GIT_PUSHED; then
  GIT_CLAUSE="git=pushed"
elif [[ "$GIT_PUSH_REFUSED" -gt 0 ]]; then
  GIT_CLAUSE="git=refused"
elif $GIT_FAILED; then
  GIT_CLAUSE="git=failed"
else
  GIT_CLAUSE="git=nothing-to-push"
fi

# Only append when git_push_allowed actually ran. If the guard never ran
# (git=skipped, git=missing, the ahead-check itself failed, or there was
# nothing to push), a 0 here would misleadingly read as "checked, found none"
# instead of "never checked".
if $GIT_GUARD_RAN; then
  GIT_CLAUSE="$GIT_CLAUSE, git_push_refused=$GIT_PUSH_REFUSED"
fi

SUMMARY="Archive: jsonl=$EXPORTED/$((EXPORTED + EXPORT_FAILED)), $GIT_CLAUSE, $DOLT_PUSH_CLAUSE, result=$RESULT"
log "$SUMMARY"
if [[ "$DOLT_PUSH_REFUSED" -gt 0 ]]; then
  log "  Refused (unsafe destination): $DOLT_REFUSAL_DETAIL"
fi
if [[ "$GIT_PUSH_REFUSED" -gt 0 ]]; then
  log "  Refused git push (unsafe destination): $GIT_REFUSAL_DETAIL"
fi

_rid="$(bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:dolt-archive,result:$RESULT \
  -d "$SUMMARY" --silent 2>/dev/null)" || true
[ -n "${_rid:-}" ] && bd close "$_rid" --reason "plugin run recorded" >/dev/null 2>&1 || true

if [[ "$EXPORT_FAILED" -gt 0 ]]; then
  escalate_once "export-failed" "$(sorted_set "$EXPORT_FAILED_DBS")" \
    "dolt-archive: JSONL export failed for $EXPORT_FAILED databases ($EXPORT_ERRORS)" \
    "JSONL is our last-resort recovery layer. Failed databases: $EXPORT_ERRORS" \
    "$(sorted_set "$EXPORT_FAILED_DBS")"
fi
clear_resolved "export-failed" "${PROD_DBS[*]}"

if ! $SKIP_DOLT_PUSH; then
  if [[ "$DOLT_PUSH_FAILED" -gt 0 ]]; then
    # Coverage set for clear_resolved is the DBs behind each failing
    # "db/remote" member, not the members themselves — a db is "checked"
    # once this cycle attempts any of its remotes.
    DOLT_PUSH_FAILED_DBS=""
    for _member in $DOLT_PUSH_FAILED_SET; do
      DOLT_PUSH_FAILED_DBS="${DOLT_PUSH_FAILED_DBS}${_member%%/*} "
    done
    escalate_once "dolt-push-failed" "$(sorted_set "$DOLT_PUSH_FAILED_SET")" \
      "dolt-archive: dolt push failed for $DOLT_PUSH_FAILED remote(s)" \
      "Native Dolt replication did not reach $DOLT_PUSH_FAILED remote(s) this cycle. The data did not leave this machine via that path." \
      "$(sorted_set "$DOLT_PUSH_FAILED_DBS")"
  fi
  clear_resolved "dolt-push-failed" "${PROD_DBS[*]}"

  if $REMOTE_SHORTFALL; then
    # Affected set = exported DBs with no remote (names, not counts, so a
    # different DB lacking one is a change even if the count is the same).
    NO_REMOTE_SET=""
    for _db in "${EXPORTED_DBS[@]:-}"; do
      [[ -z "$_db" ]] && continue
      case " $DBS_WITH_REMOTE_SET" in
        *" $_db "*) ;;
        *) NO_REMOTE_SET="${NO_REMOTE_SET}${_db} " ;;
      esac
    done
    escalate_once "remote-shortfall" "$(sorted_set "$NO_REMOTE_SET")" \
      "dolt-archive: only $EXPORTED_DBS_WITH_REMOTE of $EXPORTED exported databases have a dolt remote configured" \
      "$((EXPORTED - EXPORTED_DBS_WITH_REMOTE)) exported database(s) have no dolt remote at all, so dolt push never attempts them. The dolt_push ratio in the summary covers all $DBS_WITH_REMOTE db(s) with a remote (exported or not), not just these $EXPORTED_DBS_WITH_REMOTE exported one(s)." \
      "$(sorted_set "$NO_REMOTE_SET")"
  fi
  clear_resolved "remote-shortfall" "${PROD_DBS[*]}"
  clear_resolved "push-refused:*" "${PROD_DBS[*]}"
fi

if ! $SKIP_GIT; then
  if [[ "$GIT_PUSH_REFUSED" -gt 0 ]]; then
    escalate_once "git-push-refused:origin" "${GIT_REFUSAL_DETAIL}|${PUSH_URLS:-}" \
      "dolt-archive: refused git backup push to unsafe remote origin" \
      "Push destination visibility is '${GIT_REFUSAL_DETAIL}', not confirmed private. Refusing to push JSONL exports of the production databases from $BACKUP_REPO to ${PUSH_URLS:-<unresolvable>} until it is. See gt-sg6n."
  fi
  clear_resolved "git-push-refused:origin"

  if $GIT_FAILED; then
    escalate_once "git-backup-failed" "$BACKUP_REPO" \
      "dolt-archive: git backup add/commit/push failed" \
      "A git add, commit, or push to the backup repo at $BACKUP_REPO failed this cycle. The JSONL snapshot did not reach the offsite git backup via this path this cycle. See plugin logs for the specific git error."
  fi
  clear_resolved "git-backup-failed"

  if $GIT_REPO_MISSING; then
    escalate_once "git-repo-missing" "$BACKUP_REPO" \
      "dolt-archive: no git backup repo at $BACKUP_REPO" \
      "The git-backup path is entirely a no-op with no repo at $BACKUP_REPO — JSONL snapshots are not leaving this machine via git."
  fi
  clear_resolved "git-repo-missing"
fi

log "Done."
