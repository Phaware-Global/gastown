#!/usr/bin/env bash
# push_guard_test.sh — compactor-dog decides its push target ONCE per database,
# before any fetch, divergence check, flatten or push, through the same
# fail-closed visibility guard as dolt-archive.
#
# INVARIANT under test: a database with a configured Dolt remote is never
# flattened in a cycle that cannot push to it. If the remote exists but the
# guard refuses it for ANY reason (public, unknown, non-GitHub, DoltHub,
# unreadable URL, or a missing guard, gh or timeout), the database is skipped
# for the cycle (no fetch, no flatten, no push) and escalated once with a
# fingerprint per db and reason. A database with NO remote compacts locally.
#
# No network: gh, dolt and gt are fakes shimmed onto PATH. The functions under
# test are extracted from run.sh (not copied) so the test cannot drift from it.
#
# Usage: bash plugins/compactor-dog/push_guard_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
PASS=0
FAIL=0

FAKE_BIN="$WORK/bin"
mkdir -p "$FAKE_BIN"

# Fake gh: visibility is chosen by the owner segment of the repo path.
cat > "$FAKE_BIN/gh" <<'FAKE_GH'
#!/usr/bin/env bash
repo=""
for a in "$@"; do [[ "$a" == repos/* ]] && repo="${a#repos/}"; done
case "${repo%%/*}" in
  pub-owner)  echo "public" ;;
  priv-owner) echo "private" ;;
  *)          exit 1 ;;
esac
FAKE_GH

# Fake dolt. It answers ONLY the exact query text run.sh sends, with the column
# names real Dolt returns (dolt_remote_branches has "hash", not "commit_hash";
# its tracking refs are named "remotes/<remote>/main"). No pattern matching:
# any other query exits 2, so a query that drifts from what Dolt accepts fails
# the test instead of being answered by an invented schema. The same queries
# run against a real Dolt in compactor_realdolt_integration_test.go.
#   FAKE_NO_REMOTE=1  the database has no remote at all
#   FAKE_FAIL=<text>  any query containing <text> FAILS (exit 1, stderr) — an
#                     error, which must never be read as an empty result
#   FAKE_LOCAL_HEAD / FAKE_REMOTE_HEAD / FAKE_ANCESTOR  divergence answers
cat > "$FAKE_BIN/dolt" <<'FAKE_DOLT'
#!/usr/bin/env bash
query=""
prev=""
for a in "$@"; do
  if [[ "$prev" == "-q" ]]; then query="$a"; fi
  prev="$a"
done
if [[ -n "${FAKE_FAIL:-}" && "$query" == *"$FAKE_FAIL"* ]]; then
  echo "fake dolt: induced failure" >&2
  exit 1
fi
case "$query" in
  "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") touch "$PUSHED_MARKER"; exit 0 ;;
  "SELECT name FROM dolt_remotes LIMIT 1")
    echo "name"; [[ -z "${FAKE_NO_REMOTE:-}" ]] && echo "origin"; exit 0 ;;
  "SELECT url FROM dolt_remotes WHERE name = 'origin'")
    echo "url"; [[ -n "${FAKE_REMOTE_URL:-}" ]] && echo "$FAKE_REMOTE_URL"; exit 0 ;;
  "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1")
    echo "commit_hash"; echo "${FAKE_LOCAL_HEAD-abc123}"; exit 0 ;;
  "SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'")
    echo "hash"; [[ -n "${FAKE_REMOTE_HEAD-def456}" ]] && echo "${FAKE_REMOTE_HEAD-def456}"; exit 0 ;;
  "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = '"*"'")
    echo "COUNT(*)"; echo "${FAKE_ANCESTOR-1}"; exit 0 ;;
esac
echo "fake dolt: unexpected query: $query" >&2
exit 2
FAKE_DOLT

# Fake gt: records escalations.
cat > "$FAKE_BIN/gt" <<'FAKE_GT'
#!/usr/bin/env bash
echo "$*" >> "$ESCALATIONS"
FAKE_GT
chmod +x "$FAKE_BIN/gh" "$FAKE_BIN/dolt" "$FAKE_BIN/gt"
export PATH="$FAKE_BIN:$PATH"
export PUSHED_MARKER="$WORK/pushed"
export ESCALATIONS="$WORK/escalations"

# Pull in the guard and the functions under test.
source "$SCRIPT_DIR/../dolt-archive/visibility_guard.sh"
DOLT_HOST=127.0.0.1 DOLT_PORT=3307 DOLT_USER=root LOGFILE="$WORK/stderr.log"
: > "$LOGFILE"
log() { echo "[test] $*"; }
dolt_query() {
  local db="$1" query="$2"
  dolt --use-db "$db" sql -q "$query" --result-format csv 2>>"$LOGFILE" | tail -n +2 | tr -d '\r'
}
dolt_exec() {
  local db="$1" query="$2"
  dolt --use-db "$db" sql -q "$query" --result-format csv >/dev/null 2>>"$LOGFILE"
}

for fn in validate_name validate_hash query_first verify_remote_not_diverged load_visibility_guard resolve_push_target escalate_push_refused remote_gate push_compacted_history; do
  fn_src="$(sed -n "/^${fn}()/,/^}/p" "$SCRIPT_DIR/run.sh")"
  if [[ -z "$fn_src" ]]; then
    echo "FAIL: $fn is not defined in run.sh"
    exit 1
  fi
  eval "$fn_src"
done

# One database's remote handling exactly as run.sh's loop does it: remote_gate
# decides once; only a database that clears it is fetched, flattened, pushed.
FETCHED_MARKER="$WORK/fetched"
FLATTENED_MARKER="$WORK/flattened"
CYCLE_RC=0
cycle() {
  local url="$1"
  rm -f "$PUSHED_MARKER" "$FETCHED_MARKER" "$FLATTENED_MARKER" "$ESCALATIONS"
  HAS_REMOTE=false; REMOTE_NAME=""; REMOTE_URL=""; PUSH_REFUSED=false; PUSH_REFUSAL_REASON=""
  CYCLE_RC=0
  FAKE_REMOTE_URL="$url" remote_gate gt >"$WORK/cycle.out" 2>&1 || CYCLE_RC=$?
  if [[ "$CYCLE_RC" -eq 0 ]]; then
    if $HAS_REMOTE; then touch "$FETCHED_MARKER"; fi   # stands in for DOLT_FETCH + divergence check
    touch "$FLATTENED_MARKER"                          # stands in for the soft-reset + flatten commit
    if $HAS_REMOTE; then
      FAKE_REMOTE_URL="$url" push_compacted_history gt "$REMOTE_NAME" >>"$WORK/cycle.out" 2>&1 || true
    fi
  fi
}

# check NAME WANT URL
# WANT=compact-push : private remote -> fetched, flattened, pushed, no escalation
# WANT=skip         : remote refused  -> nothing fetched/flattened/pushed, one escalation
# WANT=local        : no remote       -> flattened only, no escalation
check() {
  local name="$1" want="$2" url="$3" fetched=0 flattened=0 pushed=0 esc refusals
  cycle "$url"
  if [[ -e "$FETCHED_MARKER" ]]; then fetched=1; fi
  if [[ -e "$FLATTENED_MARKER" ]]; then flattened=1; fi
  if [[ -e "$PUSHED_MARKER" ]]; then pushed=1; fi
  esc="$(grep -c 'escalate' "$ESCALATIONS" 2>/dev/null || true)"
  refusals="$(grep -c 'REFUSED' "$WORK/cycle.out" || true)"
  esc="${esc:-0}"
  local ok=1
  case "$want" in
    compact-push) [[ "$fetched$flattened$pushed$esc$refusals" == "11100" ]] || ok=0 ;;
    skip)         [[ "$fetched$flattened$pushed$esc$refusals" == "00011" && "$CYCLE_RC" -ne 0 ]] || ok=0 ;;
    local)        [[ "$fetched$flattened$pushed$esc$refusals" == "01000" ]] || ok=0 ;;
  esac
  if [[ "$ok" -eq 1 ]]; then
    echo "ok   - $name"; PASS=$((PASS + 1))
  else
    echo "FAIL - $name (fetched=$fetched flattened=$flattened pushed=$pushed escalations=$esc refusals=$refusals rc=$CYCLE_RC want=$want)"
    FAIL=$((FAIL + 1))
  fi
}

check "public repo: skipped, nothing flattened"          skip "git+https://github.com/pub-owner/repo"
check "unknown visibility (lookup fails): skipped"       skip "git+https://github.com/unknown-owner/repo"
check "non-GitHub remote: skipped"                       skip "https://bitbucket.org/priv-owner/repo"
check "DoltHub remote: skipped"                          skip "https://doltremoteapi.dolthub.com/priv-owner/repo"
check "unresolvable remote URL: skipped"                 skip ""
check "private repo: fetched, flattened, pushed"         compact-push "git+https://github.com/priv-owner/repo"
FAKE_NO_REMOTE=1 check "no remote: compacts locally, no escalation" local ""

# Every read in the push-target path tells "failed" from "empty". A failed
# remote-name or URL lookup is NOT "no remote": the database is skipped and
# escalated, never flattened with no push.
FAKE_FAIL="SELECT name FROM dolt_remotes" check "remote-name lookup fails: skipped, not read as no remote" skip "git+https://github.com/priv-owner/repo"
if grep -q -- 'push-refused:gt:remote-lookup-failed' "$ESCALATIONS" 2>/dev/null \
   && grep -q 'could not read' "$ESCALATIONS" && ! grep -q 'guard refused' "$ESCALATIONS"; then
  echo "ok   - failed name lookup escalates with reason remote-lookup-failed"; PASS=$((PASS + 1))
else
  echo "FAIL - failed name lookup escalation: $(cat "$ESCALATIONS" 2>/dev/null)"; FAIL=$((FAIL + 1))
fi
FAKE_FAIL="SELECT url FROM dolt_remotes" check "remote-URL lookup fails: skipped" skip "git+https://github.com/priv-owner/repo"

# The divergence reads after a fetch: verify_remote_not_diverged returns 0 to
# proceed, 10 when diverged, 11 when the check could not be made (a failed
# read is not "no divergence").
div() {  # div NAME WANT_RC
  local name="$1" want="$2" rc=0
  REMOTE_NAME=origin
  verify_remote_not_diverged gt >"$WORK/div.out" 2>&1 || rc=$?
  if [[ "$rc" -eq "$want" ]]; then echo "ok   - $name"; PASS=$((PASS + 1))
  else echo "FAIL - $name (rc=$rc want=$want)"; FAIL=$((FAIL + 1)); fi
}
div "remote HEAD is in local history: proceeds"                         0
FAKE_REMOTE_HEAD="" div "remote has no main branch yet (empty read): proceeds" 0
FAKE_REMOTE_HEAD="abc123" div "remote HEAD equals local HEAD: proceeds" 0
FAKE_ANCESTOR=0 div "remote HEAD missing from local history: diverged" 10
FAKE_FAIL="ORDER BY date DESC" div "local-HEAD read fails: cannot verify"  11
FAKE_FAIL="FROM dolt_remote_branches" div "remote-HEAD read fails: cannot verify" 11
FAKE_FAIL="COUNT(*) FROM dolt_log" div "ancestor read fails: cannot verify" 11
FAKE_LOCAL_HEAD="" div "local HEAD empty (no history read): cannot verify" 11
FAKE_REMOTE_HEAD="NOT A HASH" div "malformed remote HEAD: cannot verify" 11

# The escalation is fingerprinted per db and reason, so an open one suppresses
# repeats on later cycles.
cycle "git+https://github.com/pub-owner/repo"
if grep -q -- '--fingerprint compactor-dog:push-refused:gt:public' "$ESCALATIONS"; then
  echo "ok   - escalation fingerprint is per db and reason"; PASS=$((PASS + 1))
else
  echo "FAIL - escalation fingerprint is per db and reason: $(cat "$ESCALATIONS" 2>/dev/null)"; FAIL=$((FAIL + 1))
fi

# Environments where the guard cannot answer at all are refusals too: the
# database is skipped (not flattened) and escalated, and the script survives
# set -e.
SAVED_PATH="$PATH"
SAVED_GUARD_FN="$(declare -f remote_push_allowed)"

# Build a PATH holding the fakes plus only the named system tools.
restricted_path() {
  local dir="$1"; shift
  mkdir -p "$dir"
  local tool p
  for tool in bash dirname tr head tail cat touch rm grep sed "$@"; do
    p="$(command -v "$tool" 2>/dev/null || true)"
    if [[ -n "$p" ]]; then ln -sf "$p" "$dir/$tool"; fi
  done
  ln -sf "$FAKE_BIN/dolt" "$dir/dolt"
  ln -sf "$FAKE_BIN/gt" "$dir/gt"
}

# gh missing: timeout is present, gh is not.
restricted_path "$WORK/nogh" timeout
PATH="$WORK/nogh" check "gh missing: skipped, nothing flattened" skip "git+https://github.com/priv-owner/repo"
PATH="$SAVED_PATH"

# timeout missing: gh answers but the guard's timeout wrapper does not exist.
restricted_path "$WORK/noto"
ln -sf "$FAKE_BIN/gh" "$WORK/noto/gh"
PATH="$WORK/noto" check "timeout missing: skipped, nothing flattened" skip "git+https://github.com/priv-owner/repo"
PATH="$SAVED_PATH"

# Guard file missing: load_visibility_guard must leave a refuse-everything stub.
load_visibility_guard "$WORK/does-not-exist.sh"
check "guard missing: skipped, nothing flattened" skip "git+https://github.com/priv-owner/repo"
eval "$SAVED_GUARD_FN"

# push_compacted_history alone never pushes to a remote that was not resolved
# as the push target.
rm -f "$PUSHED_MARKER"; HAS_REMOTE=false; REMOTE_NAME=""
if FAKE_REMOTE_URL="git+https://github.com/priv-owner/repo" push_compacted_history gt origin >/dev/null 2>&1 || [[ -e "$PUSHED_MARKER" ]]; then
  echo "FAIL - push without a resolved target must refuse"; FAIL=$((FAIL + 1))
else
  echo "ok   - push without a resolved target refuses"; PASS=$((PASS + 1))
fi

echo ""
echo "push_guard_test.sh: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
