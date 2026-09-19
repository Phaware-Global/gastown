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
#   5. persistent condition, 2 cycles -> must escalate exactly once (fingerprint dedup)
#   6. exported db with no remote, unexported db with a remote -> shortfall still caught
#   7. git push failure (repo present) -> must escalate critical
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
#
# A per-db marker file $DOLT_DATA_DIR/<db>/.mock-no-issues-table makes the
# "SHOW TABLES LIKE 'issues'" query report no table for that db specifically,
# so a scenario can simulate "this db was never exported" independently of
# whether it has a dolt remote configured.
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

# Otherwise it's the `--host ... [--use-db DB] sql -q "<query>" --result-format <fmt>` form.
query=""
db=""
prev=""
for arg in "$@"; do
  if [[ "$prev" == "-q" ]]; then
    query="$arg"
  fi
  if [[ "$prev" == "--use-db" ]]; then
    db="$arg"
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
    if [[ ! -f "${DOLT_DATA_DIR:-$HOME/gt/.dolt-data}/$db/.mock-no-issues-table" ]]; then
      printf 'issues\n'
    fi
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

# gt mock: simulates the real `gt escalate --fingerprint` duplicate-suppression
# behavior (escalate_impl.go) closely enough to test it — a fingerprint seen
# before in this sandbox produces no new escalate.log line at all, exactly
# like the real command creating no new bead. State lives in
# $sandbox/.fingerprints so it persists across separate run.sh invocations
# within the same sandbox (simulating separate archive cycles).
write_gt_mock() {
  local sandbox="$1"
  cat > "$sandbox/bin/gt" <<MOCK
#!/usr/bin/env bash
if [[ "\$1" == "escalate" ]]; then
  fp=""
  prev=""
  for arg in "\$@"; do
    if [[ "\$prev" == "--fingerprint" ]]; then fp="\$arg"; fi
    prev="\$arg"
  done
  if [[ -n "\$fp" ]]; then
    mkdir -p "$sandbox/.fingerprints"
    seen_file="$sandbox/.fingerprints/\${fp//[^A-Za-z0-9_.-]/_}"
    if [[ -f "\$seen_file" ]]; then
      exit 0
    fi
    touch "\$seen_file"
  fi
  printf '%s\n' "\$*" >> "$sandbox/escalate.log"
  exit 0
fi
exit 0
MOCK
  chmod +x "$sandbox/bin/gt"
}

# Builds a real (non-mocked) git backup repo with an origin remote already
# tracked as refs/remotes/origin/main and a pre-existing committed
# testdb.jsonl. run.sh's own `git` calls run for real against this repo —
# only `dolt`/`bd`/`gt` are mocked — so overwriting testdb.jsonl with
# different mocked export content produces a genuine diff to commit, and
# breaking the remote afterward produces a genuine push failure.
write_git_backup_repo() {
  local sandbox="$1"
  local repo="$sandbox/home/gt/.dolt-archive/git"
  local bare="$sandbox/bare-origin.git"
  git init --quiet --bare "$bare"
  git init --quiet -b main "$repo"
  (
    cd "$repo"
    git config user.email "test@example.invalid"
    git config user.name "Test"
    git remote add origin "$bare"
    printf '{"id":"old"}\n' > testdb.jsonl
    git add testdb.jsonl
    git commit --quiet -m "seed"
    git push --quiet -u origin main
  )
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

# Asserts the escalate.log line matching $needle carries a stable
# --fingerprint keyed to the condition (no per-run data like counts/dates).
assert_fingerprint() {
  local sandbox="$1" needle="$2" fingerprint="$3" desc="$4"
  local line
  line="$(grep -- "$needle" "$sandbox/escalate.log" 2>/dev/null || true)"
  if [[ -z "$line" ]]; then
    echo "FAIL: $desc — no escalation matching '$needle' to check fingerprint on"
    FAILURES=$((FAILURES + 1))
    return
  fi
  if ! grep -q -- "--fingerprint $fingerprint" <<< "$line"; then
    echo "FAIL: $desc — expected --fingerprint $fingerprint, got: $line"
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
assert_fingerprint "$SANDBOX" "dolt push failed" "dolt-archive:dolt-push-failed" "dolt push failure"
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
assert_fingerprint "$SANDBOX" "no git backup repo" "dolt-archive:git-repo-missing" "missing git backup repo"
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
assert_fingerprint "$SANDBOX" "only 1 of 2 exported databases" "dolt-archive:remote-shortfall" "remote-count shortfall"
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

# --- Scenario 5: a persistent condition across two cycles must escalate ------
# --- exactly once (fingerprint dedup), not once per cycle --------------------

log "=== Scenario: persistent condition dedupes across cycles ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"

DB_DIR="$SANDBOX/home/gt/.dolt-data/testdb"
mkdir -p "$DB_DIR/.dolt"
printf 'origin\thttps://example.invalid/testdb (fetch)\n' > "$DB_DIR/.mock-remotes"
touch "$DB_DIR/.mock-push-fail-origin"

# Deliberately do NOT use run_scenario here — it truncates escalate.log on
# each call, which would hide duplicate escalations across cycles. Two
# consecutive invocations against the same sandbox simulate two archive
# cycles hitting the same persistent (never-fixed) push failure.
: > "$SANDBOX/escalate.log"
for cycle in 1 2; do
  (
    export HOME="$SANDBOX/home"
    export PATH="$SANDBOX/bin:$PATH"
    export DOLT_DATA_DIR="$SANDBOX/home/gt/.dolt-data"
    "$RUN_SH" --databases testdb --skip-git
  ) > "$SANDBOX/output-cycle$cycle.log" 2>&1 || true
done

COUNT="$(grep -c -- "dolt push failed" "$SANDBOX/escalate.log" 2>/dev/null || true)"
COUNT="${COUNT:-0}"
if [[ "$COUNT" -ne 1 ]]; then
  echo "FAIL: persistent condition across 2 cycles — expected exactly 1 escalation total, got $COUNT"
  echo "  escalate.log contents:"
  sed 's/^/    /' "$SANDBOX/escalate.log" 2>/dev/null || echo "    (empty)"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

# --- Scenario 6: an exported db with no remote must not be masked by an ------
# --- unexported db that happens to have one (population correctness) --------

log "=== Scenario: population correctness (exported-without-remote not masked) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"

# db-a: exported (has an issues table) but has NO remote configured.
DB_A_DIR="$SANDBOX/home/gt/.dolt-data/db-a"
mkdir -p "$DB_A_DIR/.dolt"

# db-b: NOT exported (no issues table) but DOES have a remote configured.
# Before the fix, db-b's remote alone made DBS_WITH_REMOTE (1) equal
# EXPORTED (1, db-a only), hiding that the actually-exported db has none.
DB_B_DIR="$SANDBOX/home/gt/.dolt-data/db-b"
mkdir -p "$DB_B_DIR/.dolt"
printf 'origin\thttps://example.invalid/db-b (fetch)\n' > "$DB_B_DIR/.mock-remotes"
touch "$DB_B_DIR/.mock-no-issues-table"
# Pre-existing, unrelated bug (filed separately, not this bead's scope): the
# prune loop globs "${DB}-2*.jsonl" for every PROD_DBS entry including ones
# skipped this cycle; with no snapshot ever written for db-b, the glob
# matches nothing and (under set -eo pipefail) aborts the whole script. Seed
# a dummy snapshot so db-b's prune-loop iteration is a no-op and this
# scenario can exercise the export/remote population logic it's actually for.
touch "$SANDBOX/home/gt/.dolt-archive/jsonl/db-b-20200101-0000.jsonl"

run_scenario "$SANDBOX" --databases db-a,db-b --skip-git
assert_escalated "$SANDBOX" "only 0 of 1 exported databases have a dolt remote configured" "population correctness"
rm -rf "$SANDBOX"

# --- Scenario 7: git push failure (repo present) must escalate critical -----

log "=== Scenario: git push failure ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"

# Break the remote AFTER origin/main is already tracked locally, so the
# rev-list ahead-check still works but the actual push fails.
git -C "$SANDBOX/home/gt/.dolt-archive/git" remote set-url origin "/nonexistent/broken-origin-$$.git"

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_escalated "$SANDBOX" "git backup add/commit/push failed" "git push failure"
assert_fingerprint "$SANDBOX" "git backup add/commit/push failed" "dolt-archive:git-backup-failed" "git push failure"
rm -rf "$SANDBOX"

# --- Summary -------------------------------------------------------------------

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES scenario(s) failed"
  exit 1
else
  echo "PASSED: all scenarios passed"
fi
