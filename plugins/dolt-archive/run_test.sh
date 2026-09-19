#!/usr/bin/env bash
# Integration tests for dolt-archive/run.sh escalation logic (gt-igzm).
#
# Runs run.sh end-to-end against mocked `dolt`, `bd`, and `gt` binaries so we
# can assert on which escalations actually fire, not just on internal
# counters. Each scenario isolates one condition:
#   1. dolt push failure          -> must escalate critical
#   2. missing git backup repo    -> must escalate critical
#   3. remote count < exported    -> must escalate critical
#   4. everything healthy         -> must NOT escalate at all
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SH="$SCRIPT_DIR/run.sh"
FAILURES=0

log() { echo "[test] $*"; }

# --- Mock binary factory ------------------------------------------------------
#
# Builds a fake $HOME with a fake `bin/` on PATH containing `dolt`, `bd`, and
# `gt` mocks, plus the directory layout run.sh expects under $HOME/gt/...
# Every `gt escalate` invocation is appended as one line to $ESCALATE_LOG.

setup_sandbox() {
  local sandbox
  sandbox="$(mktemp -d)"
  mkdir -p "$sandbox/bin"
  mkdir -p "$sandbox/home/gt/.dolt-archive/jsonl"
  mkdir -p "$sandbox/home/gt/.dolt-data"
  echo "$sandbox"
}

# dolt mock: understands the three call shapes run.sh makes.
#   dolt --host H --port P --no-tls -u U -p "" [--use-db DB] sql -q "<query>" --result-format <fmt>
#   dolt remote -v                          (run with cwd == a db dir)
#   dolt push <remote> main                 (run with cwd == a db dir)
write_dolt_mock() {
  local sandbox="$1"
  cat > "$sandbox/bin/dolt" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$1" == "remote" && "$2" == "-v" ]]; then
  if [[ -f ".mock-remotes" ]]; then cat ".mock-remotes"; fi
  exit 0
fi

if [[ "$1" == "push" ]]; then
  remote="$2"
  if [[ -f ".mock-push-fail-$remote" ]]; then
    echo "mock push failure for $remote" >&2
    exit 1
  fi
  echo "pushed to $remote"
  exit 0
fi

# Otherwise it's the `--host ... sql -q "<query>" --result-format <fmt>` form.
query=""
prev=""
for arg in "$@"; do
  if [[ "$prev" == "-q" ]]; then
    query="$arg"
  fi
  prev="$arg"
done

case "$query" in
  "SHOW DATABASES")
    printf 'Database\n'
    [[ -f "$MOCK_DB_LIST" ]] && cat "$MOCK_DB_LIST"
    ;;
  "SHOW TABLES LIKE 'issues'")
    printf 'Tables_in_db\n'
    printf 'issues\n'
    ;;
  "SELECT * FROM issues ORDER BY id")
    printf '{"id":"1"}\n'
    ;;
  *)
    echo "dolt mock: unrecognized query: $query" >&2
    exit 1
    ;;
esac
MOCK
  chmod +x "$sandbox/bin/dolt"
}

write_bd_mock() {
  local sandbox="$1"
  cat > "$sandbox/bin/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  create) echo "mock-receipt-$$" ;;
  *) : ;;
esac
exit 0
MOCK
  chmod +x "$sandbox/bin/bd"
}

write_gt_mock() {
  local sandbox="$1"
  cat > "$sandbox/bin/gt" <<MOCK
#!/usr/bin/env bash
if [[ "\$1" == "escalate" ]]; then
  printf '%s\n' "\$*" >> "$sandbox/escalate.log"
  exit 0
fi
exit 0
MOCK
  chmod +x "$sandbox/bin/gt"
}

run_scenario() {
  local sandbox="$1"; shift
  : > "$sandbox/escalate.log"
  (
    export HOME="$sandbox/home"
    export PATH="$sandbox/bin:$PATH"
    export DOLT_DATA_DIR="$sandbox/home/gt/.dolt-data"
    "$RUN_SH" "$@"
  ) > "$sandbox/output.log" 2>&1 || true
}

assert_escalated() {
  local sandbox="$1" needle="$2" desc="$3"
  if ! grep -q -- "$needle" "$sandbox/escalate.log" 2>/dev/null; then
    echo "FAIL: $desc — expected an escalation matching '$needle'"
    echo "  escalate.log contents:"
    sed 's/^/    /' "$sandbox/escalate.log" 2>/dev/null || echo "    (empty)"
    FAILURES=$((FAILURES + 1))
    return
  fi
  if ! grep -- "$needle" "$sandbox/escalate.log" | grep -q -- "-s critical"; then
    echo "FAIL: $desc — escalation for '$needle' did not use '-s critical'"
    FAILURES=$((FAILURES + 1))
  fi
}

assert_no_escalation() {
  local sandbox="$1" desc="$2"
  if [[ -s "$sandbox/escalate.log" ]]; then
    echo "FAIL: $desc — expected no escalation, but got:"
    sed 's/^/    /' "$sandbox/escalate.log"
    FAILURES=$((FAILURES + 1))
  fi
}

# --- Scenario 1: dolt push failure must escalate critical --------------------

log "=== Scenario: dolt push failure ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"

DB_DIR="$SANDBOX/home/gt/.dolt-data/testdb"
mkdir -p "$DB_DIR/.dolt"
printf 'origin\thttps://example.invalid/testdb (fetch)\n' > "$DB_DIR/.mock-remotes"
touch "$DB_DIR/.mock-push-fail-origin"

run_scenario "$SANDBOX" --databases testdb --skip-git
assert_escalated "$SANDBOX" "dolt push failed" "dolt push failure"
rm -rf "$SANDBOX"

# --- Scenario 2: missing git backup repo must escalate critical --------------

log "=== Scenario: missing git backup repo ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
# Deliberately do NOT create $HOME/gt/.dolt-archive/git.

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_escalated "$SANDBOX" "no git backup repo" "missing git backup repo"
rm -rf "$SANDBOX"

# --- Scenario 3: remote count below exported count must escalate critical ----

log "=== Scenario: remote count < exported count ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"

# db-with-remote: has a configured remote and a successful push.
WITH_REMOTE_DIR="$SANDBOX/home/gt/.dolt-data/db-with-remote"
mkdir -p "$WITH_REMOTE_DIR/.dolt"
printf 'origin\thttps://example.invalid/with-remote (fetch)\n' > "$WITH_REMOTE_DIR/.mock-remotes"

# db-no-remote: exported successfully but has no remote configured at all.
NO_REMOTE_DIR="$SANDBOX/home/gt/.dolt-data/db-no-remote"
mkdir -p "$NO_REMOTE_DIR/.dolt"

run_scenario "$SANDBOX" --databases db-with-remote,db-no-remote --skip-git
assert_escalated "$SANDBOX" "only 1 of 2 exported databases have a dolt remote configured" "remote-count shortfall"
rm -rf "$SANDBOX"

# --- Scenario 4: fully healthy run must NOT escalate --------------------------

log "=== Scenario: healthy run (no false positives) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"

HEALTHY_DIR="$SANDBOX/home/gt/.dolt-data/testdb"
mkdir -p "$HEALTHY_DIR/.dolt"
printf 'origin\thttps://example.invalid/testdb (fetch)\n' > "$HEALTHY_DIR/.mock-remotes"

run_scenario "$SANDBOX" --databases testdb --skip-git
assert_no_escalation "$SANDBOX" "healthy run"
rm -rf "$SANDBOX"

# --- Summary -------------------------------------------------------------------

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES scenario(s) failed"
  exit 1
else
  echo "PASSED: all scenarios passed"
fi
