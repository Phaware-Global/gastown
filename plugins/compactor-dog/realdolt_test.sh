#!/usr/bin/env bash
# realdolt_test.sh — runs run.sh's push-target and divergence functions against
# a REAL Dolt, so the SQL they send is checked against Dolt's real schema (the
# fake dolt in push_guard_test.sh answers whatever text it is taught and cannot
# notice drift: rounds 3-5 missed a query naming dolt_remote_branches.commit_hash,
# a column that does not exist, and a ref name Dolt does not use).
#
# Starts a throwaway `dolt sql-server` on a free loopback port over a temp data
# dir, with file:// remotes in temp dirs. It never touches the town's Dolt
# server (port 3307) and never pushes anywhere off this machine. Skips (exit 0)
# when the dolt binary is not installed; CI installs it.
#
# Usage: bash plugins/compactor-dog/realdolt_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! command -v dolt >/dev/null 2>&1; then
  echo "SKIP: dolt binary not installed"
  exit 0
fi

WORK="$(mktemp -d)"
SERVER_PID=""
stop_server() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""
  fi
}
trap 'stop_server; rm -rf "$WORK"' EXIT
PASS=0
FAIL=0

export DOLT_ROOT_PATH="$WORK/doltroot"
mkdir -p "$DOLT_ROOT_PATH"
dolt config --global --add user.name compactor-test >/dev/null
dolt config --global --add user.email compactor-test@example.com >/dev/null

# Helpers and functions under test come from run.sh itself.
log() { echo "[test] $*"; }
LOGFILE="$WORK/stderr.log"; : > "$LOGFILE"
DOLT_HOST=127.0.0.1 DOLT_USER=root DOLT_PORT=0
for fn in dolt_query dolt_exec validate_name validate_hash query_first verify_remote_not_diverged \
          load_visibility_guard resolve_push_target escalate_push_refused remote_gate push_compacted_history; do
  fn_src="$(sed -n "/^${fn}()/,/^}/p" "$SCRIPT_DIR/run.sh")"
  if [[ -z "$fn_src" ]]; then echo "FAIL: $fn is not defined in run.sh"; exit 1; fi
  eval "$fn_src"
done
# The guard is the real one (a file:// remote is not a private GitHub repo, so
# it is refused); tests that need an allowed remote override the verdict only.
load_visibility_guard "$SCRIPT_DIR/../dolt-archive/visibility_guard.sh"
REAL_GUARD="$(declare -f remote_push_allowed)"
allow_all() { remote_push_allowed() { echo "private"; return 0; }; }
real_guard() { eval "$REAL_GUARD"; }
# Escalations would call gt; record instead.
gt() { echo "$*" >> "$WORK/escalations"; }

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# new_db WITH_REMOTE(0|1) REMOTE_AHEAD(0|1): build database "gt" on disk and
# start a server over it. Sets DOLT_PORT, DB_DIR, REMOTE_DIR.
new_db() {
  local with_remote="$1" remote_ahead="$2" data
  stop_server
  data="$(mktemp -d "$WORK/data.XXXXXX")"
  DB_DIR="$data/gt"; mkdir -p "$DB_DIR"
  (
    cd "$DB_DIR"
    dolt init >/dev/null
    dolt sql -q "CREATE TABLE issues (id INT PRIMARY KEY, title VARCHAR(100))"
    dolt add . >/dev/null; dolt commit -m "create issues" >/dev/null
    for i in 1 2 3; do
      dolt sql -q "INSERT INTO issues VALUES ($i, 'issue $i')"
      dolt add . >/dev/null; dolt commit -m "issue $i" >/dev/null
    done
  )
  REMOTE_DIR=""
  if [[ "$with_remote" -eq 1 ]]; then
    REMOTE_DIR="$(mktemp -d "$WORK/remote.XXXXXX")"
    (cd "$DB_DIR" && dolt remote add origin "file://$REMOTE_DIR" && dolt push origin main >/dev/null 2>&1)
    if [[ "$remote_ahead" -eq 1 ]]; then
      local other; other="$(mktemp -d "$WORK/other.XXXXXX")"
      (
        cd "$other" && dolt clone "file://$REMOTE_DIR" clone >/dev/null 2>&1
        cd clone
        dolt sql -q "INSERT INTO issues VALUES (9999, 'only on the remote')"
        dolt add . >/dev/null; dolt commit -m "remote-only commit" >/dev/null
        dolt push origin main >/dev/null 2>&1
      )
    fi
  fi
  DOLT_PORT="$(free_port)"
  if [[ "$DOLT_PORT" == "3307" ]]; then echo "refusing the town Dolt port"; exit 1; fi
  (cd "$data" && exec dolt sql-server --host 127.0.0.1 --port "$DOLT_PORT" >"$WORK/server.log" 2>&1) &
  SERVER_PID=$!
  local i
  for i in $(seq 1 100); do
    if dolt --host 127.0.0.1 --port "$DOLT_PORT" --no-tls -u root -p "" --use-db gt sql -q "SELECT 1" >/dev/null 2>&1; then return 0; fi
    sleep 0.3
  done
  echo "FAIL: dolt sql-server did not come up"; cat "$WORK/server.log"; exit 1
}

local_hash()  { dolt_query gt "SELECT DOLT_HASHOF('main')" | head -1; }
tracked_hash() { dolt_query gt "SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'" | head -1; }

ok()   { echo "ok   - $1"; PASS=$((PASS + 1)); }
bad()  { echo "FAIL - $1"; FAIL=$((FAIL + 1)); }

# 1. No remote: a successful empty read is "no remote", not a refusal.
new_db 0 0
real_guard
resolve_push_target gt
if ! $HAS_REMOTE && ! $PUSH_REFUSED; then ok "no remote: no target, not refused (compacts locally)"; else bad "no remote: HAS_REMOTE=$HAS_REMOTE PUSH_REFUSED=$PUSH_REFUSED"; fi

# 2. Real guard on a file:// remote: refused (non-github-remote).
new_db 1 0
real_guard
resolve_push_target gt
if $PUSH_REFUSED && [[ "$PUSH_REFUSAL_REASON" == "non-github-remote" ]] && ! $HAS_REMOTE; then
  ok "file:// remote: refused by the real guard (non-github-remote)"
else bad "file:// remote: PUSH_REFUSED=$PUSH_REFUSED reason=$PUSH_REFUSAL_REASON"; fi
rm -f "$WORK/escalations"
rc=0; remote_gate gt >/dev/null 2>&1 || rc=$?
if [[ "$rc" -eq 3 ]] && grep -q 'push-refused:gt:non-github-remote' "$WORK/escalations"; then
  ok "refused remote: remote_gate skips the database and escalates once"
else bad "remote_gate rc=$rc escalations=$(cat "$WORK/escalations" 2>/dev/null)"; fi

# 3. Allowed remote in sync: target resolved; divergence check passes on real
# dolt_remote_branches; force-push lands the remote on the local HEAD.
new_db 1 0
allow_all
resolve_push_target gt
if $HAS_REMOTE && [[ "$REMOTE_NAME" == "origin" ]]; then ok "allowed remote: push target resolved"; else bad "allowed remote: HAS_REMOTE=$HAS_REMOTE"; fi
dolt_exec gt "CALL DOLT_FETCH('origin')"
rc=0; verify_remote_not_diverged gt >/dev/null 2>&1 || rc=$?
if [[ "$rc" -eq 0 ]]; then ok "in sync: divergence check passes against real Dolt"; else bad "in sync: verify_remote_not_diverged rc=$rc"; fi
# Flatten (soft-reset to root, one commit) then force-push, as run.sh does.
root="$(dolt_query gt "SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1" | head -1)"
dolt_exec gt "CALL DOLT_RESET('--soft', '$root')"
dolt_exec gt "CALL DOLT_COMMIT('-Am', 'compaction: flatten gt history to single commit')"
if push_compacted_history gt origin >/dev/null 2>&1; then ok "force-push succeeds against a real file:// remote"; else bad "force-push failed"; fi
dolt_exec gt "CALL DOLT_FETCH('origin')"
if [[ -n "$(local_hash)" && "$(local_hash)" == "$(tracked_hash)" ]]; then ok "remote main equals the compacted local main"; else bad "remote $(tracked_hash) != local $(local_hash)"; fi

# 4. Remote ahead: diverged (10), for the right reason, not a query error.
new_db 1 1
allow_all
resolve_push_target gt
dolt_exec gt "CALL DOLT_FETCH('origin')"
rc=0; verify_remote_not_diverged gt >/dev/null 2>&1 || rc=$?
if [[ "$rc" -eq 10 ]]; then ok "remote ahead: reads as diverged (10)"; else bad "remote ahead: verify_remote_not_diverged rc=$rc (want 10)"; fi

# 5. A failed lookup is a refusal, not "no remote": point at a port nothing
# listens on, so the real client fails.
stop_server
DOLT_PORT="$(free_port)"
real_guard
resolve_push_target gt
if $PUSH_REFUSED && [[ "$PUSH_REFUSAL_REASON" == "remote-lookup-failed" ]]; then
  ok "unreachable server: remote-lookup-failed refusal, not no-remote"
else bad "unreachable server: PUSH_REFUSED=$PUSH_REFUSED reason=$PUSH_REFUSAL_REASON"; fi

echo ""
echo "realdolt_test.sh: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
