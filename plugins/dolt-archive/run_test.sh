#!/usr/bin/env bash
# run_test.sh — integration tests for dolt-archive/run.sh.
#
# Two independent suites, run back-to-back, neither replacing the other:
#   Suite A (gt-igzm): escalation logic — mocked dolt/bd/gt, asserts on which
#     escalations fire (dolt push failure, missing git backup repo, remote
#     shortfall, git push failure/success/nothing-to-push, fingerprint dedup).
#   Suite B (gt-v3df): the Dolt-push visibility guard — runs the REAL run.sh
#     against a scratch DOLT_DATA_DIR with fake dolt/gh/bd/gt binaries,
#     asserting a public remote is refused and a private one is pushed.
#
# Usage: bash plugins/dolt-archive/run_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_SH="$SCRIPT_DIR/run.sh"

log() { echo "[test] $*"; }

# =============================================================================
# Suite A: escalation logic (gt-igzm)
#
#   1. dolt push failure          -> must escalate critical
#   2. missing git backup repo    -> must escalate critical
#   3. remote count < exported    -> must escalate critical
#   4. everything healthy         -> must NOT escalate at all
#   5. persistent condition, 2 cycles -> must escalate exactly once (fingerprint dedup)
#   6. exported db with no remote, unexported db with a remote -> shortfall still caught
#   7. git push failure (repo present) -> must escalate critical
#   8. successful git push (repo present, real diff)  -> summary reads git=pushed
#   9. nothing to push (repo present, no diff, nothing ahead) -> summary reads git=nothing-to-push
#  10-11. origin remote entirely absent (with/without a stale tracking ref) -> must
#     escalate as a failure, NOT read as the all-clear (gt-a7um)
# =============================================================================

FAILURES=0

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

# gh mock: always reports "private", since Suite A's remotes exist only to
# drive the mocked dolt push/failure paths, not to exercise visibility
# outcomes (that's Suite B's job) — every Suite A remote must clear
# visibility_guard.sh's gate so the scenarios below reach the mocked `dolt
# push` at all.
write_gh_mock() {
  local sandbox="$1"
  cat > "$sandbox/bin/gh" <<'MOCK'
#!/usr/bin/env bash
if [[ "$1" == "api" ]]; then
  echo "private"
  exit 0
fi
exit 1
MOCK
  chmod +x "$sandbox/bin/gh"
}

# Builds a real (non-mocked) git backup repo with an origin remote already
# tracked as refs/remotes/origin/main and a pre-existing committed
# testdb.jsonl. run.sh's own `git` calls run for real against this repo —
# only `dolt`/`bd`/`gt` are mocked — so overwriting testdb.jsonl with
# different mocked export content produces a genuine diff to commit, and
# breaking the remote afterward produces a genuine push failure.
# $2 (optional) is the content seeded into testdb.jsonl, default "{"id":"old"}"
# (differs from the dolt mock's exported "{"id":"1"}", so a scenario gets a
# real diff to commit unless it passes a matching seed to test the opposite —
# a repo already at parity with what this cycle would export.
write_git_backup_repo() {
  local sandbox="$1"
  local seed_content="${2:-}"
  [[ -z "$seed_content" ]] && seed_content='{"id":"old"}'
  local repo="$sandbox/home/gt/.dolt-archive/git"
  local bare="$sandbox/bare-origin.git"
  git init --quiet --bare "$bare"
  git init --quiet -b main "$repo"
  (
    cd "$repo"
    git config user.email "test@example.invalid"
    git config user.name "Test"
    git remote add origin "$bare"
    printf '%s\n' "$seed_content" > testdb.jsonl
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

# Asserts run.sh's own stdout/stderr (the actual "Archive: ..." summary line,
# among other log output) contains an exact substring — not just that some
# internal counter came out right, but that the text a human or the mayor
# actually reads says the right thing.
assert_output_contains() {
  local sandbox="$1" needle="$2" desc="$3"
  if ! grep -qF -- "$needle" "$sandbox/output.log" 2>/dev/null; then
    echo "FAIL: $desc — expected output.log to contain '$needle'"
    echo "  output.log contents:"
    sed 's/^/    /' "$sandbox/output.log" 2>/dev/null || echo "    (empty)"
    FAILURES=$((FAILURES + 1))
  fi
}

# Asserts the escalate.log line matching $needle also contains $reason_text
# in its --reason argument — catching drift between the numbers an escalation
# claims and the numbers it actually computed.
assert_reason_contains() {
  local sandbox="$1" needle="$2" reason_text="$3" desc="$4"
  local line
  line="$(grep -- "$needle" "$sandbox/escalate.log" 2>/dev/null || true)"
  if [[ -z "$line" ]]; then
    echo "FAIL: $desc — no escalation matching '$needle' to check reason text on"
    FAILURES=$((FAILURES + 1))
    return
  fi
  if [[ "$line" != *"$reason_text"* ]]; then
    echo "FAIL: $desc — expected reason to contain '$reason_text', got: $line"
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
write_gh_mock "$SANDBOX"

DB_DIR="$SANDBOX/home/gt/.dolt-data/testdb"
mkdir -p "$DB_DIR/.dolt"
printf 'origin\thttps://github.com/test-owner/testdb (fetch)\n' > "$DB_DIR/.mock-remotes"
touch "$DB_DIR/.mock-push-fail-origin"

run_scenario "$SANDBOX" --databases testdb --skip-git
assert_escalated "$SANDBOX" "dolt push failed" "dolt push failure"
assert_fingerprint "$SANDBOX" "dolt push failed" "dolt-archive:dolt-push-failed" "dolt push failure"
assert_output_contains "$SANDBOX" \
  "dolt_push=0/1 attempted across 1 db(s) with a remote" \
  "dolt push failure — dolt_push summary clause (attempted case)"
assert_output_contains "$SANDBOX" "git=skipped" "dolt push failure — git clause (--skip-git)"
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
assert_output_contains "$SANDBOX" "git=missing" "missing git backup repo — git clause"
assert_output_contains "$SANDBOX" "dolt_push=skipped" "missing git backup repo — dolt_push summary clause (skipped case)"
rm -rf "$SANDBOX"

# --- Scenario 3: remote count below exported count must escalate critical ----

log "=== Scenario: remote count < exported count ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_gh_mock "$SANDBOX"

# db-with-remote: has a configured remote and a successful push.
WITH_REMOTE_DIR="$SANDBOX/home/gt/.dolt-data/db-with-remote"
mkdir -p "$WITH_REMOTE_DIR/.dolt"
printf 'origin\thttps://github.com/test-owner/with-remote (fetch)\n' > "$WITH_REMOTE_DIR/.mock-remotes"

# db-no-remote: exported successfully but has no remote configured at all.
NO_REMOTE_DIR="$SANDBOX/home/gt/.dolt-data/db-no-remote"
mkdir -p "$NO_REMOTE_DIR/.dolt"

run_scenario "$SANDBOX" --databases db-with-remote,db-no-remote --skip-git
assert_escalated "$SANDBOX" "only 1 of 2 exported databases have a dolt remote configured" "remote-count shortfall"
assert_fingerprint "$SANDBOX" "only 1 of 2 exported databases" "dolt-archive:remote-shortfall" "remote-count shortfall"
assert_reason_contains "$SANDBOX" "only 1 of 2 exported databases" \
  "1 exported database(s) have no dolt remote at all, so dolt push never attempts them." \
  "remote-count shortfall — escalation reason text"
rm -rf "$SANDBOX"

# --- Scenario 4: fully healthy run must NOT escalate --------------------------

log "=== Scenario: healthy run (no false positives) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_gh_mock "$SANDBOX"

HEALTHY_DIR="$SANDBOX/home/gt/.dolt-data/testdb"
mkdir -p "$HEALTHY_DIR/.dolt"
printf 'origin\thttps://github.com/test-owner/testdb (fetch)\n' > "$HEALTHY_DIR/.mock-remotes"

run_scenario "$SANDBOX" --databases testdb --skip-git
assert_no_escalation "$SANDBOX" "healthy run"
assert_output_contains "$SANDBOX" \
  "dolt_push=1/1 attempted across 1 db(s) with a remote" \
  "healthy run — dolt_push summary clause (attempted case, all succeeded)"
assert_output_contains "$SANDBOX" "git=skipped" "healthy run — git clause (--skip-git)"
rm -rf "$SANDBOX"

# --- Scenario 5: a persistent condition across two cycles must escalate ------
# --- exactly once (fingerprint dedup), not once per cycle --------------------

log "=== Scenario: persistent condition dedupes across cycles ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_gh_mock "$SANDBOX"

DB_DIR="$SANDBOX/home/gt/.dolt-data/testdb"
mkdir -p "$DB_DIR/.dolt"
printf 'origin\thttps://github.com/test-owner/testdb (fetch)\n' > "$DB_DIR/.mock-remotes"
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
write_gh_mock "$SANDBOX"

# db-a: exported (has an issues table) but has NO remote configured.
DB_A_DIR="$SANDBOX/home/gt/.dolt-data/db-a"
mkdir -p "$DB_A_DIR/.dolt"

# db-b: NOT exported (no issues table) but DOES have a remote configured.
# Before the fix, db-b's remote alone made DBS_WITH_REMOTE (1) equal
# EXPORTED (1, db-a only), hiding that the actually-exported db has none.
DB_B_DIR="$SANDBOX/home/gt/.dolt-data/db-b"
mkdir -p "$DB_B_DIR/.dolt"
printf 'origin\thttps://github.com/test-owner/db-b (fetch)\n' > "$DB_B_DIR/.mock-remotes"
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
assert_output_contains "$SANDBOX" "git=failed" "git push failure — git clause"
rm -rf "$SANDBOX"

# --- Scenario 8: successful git push must report git=pushed, not the same ----
# --- token as skipped/missing/failed -----------------------------------------

log "=== Scenario: successful git push (git=pushed) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_no_escalation "$SANDBOX" "successful git push"
assert_output_contains "$SANDBOX" "git=pushed" "successful git push — git clause"
rm -rf "$SANDBOX"

# --- Scenario 9: nothing to push (repo present, already at parity) must ------
# --- report git=nothing-to-push, not the same token as a failure -------------

log "=== Scenario: nothing to push (git=nothing-to-push) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
# Seed the repo with content matching what this cycle will export, and
# already pushed, so there is nothing to commit and nothing ahead of origin.
write_git_backup_repo "$SANDBOX" '{"id":"1"}'

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_no_escalation "$SANDBOX" "nothing to push"
assert_output_contains "$SANDBOX" "git=nothing-to-push" "nothing to push — git clause"
rm -rf "$SANDBOX"

# --- Scenario 10: origin remote entirely absent, with an unpushed commit, ----
# --- must escalate as a failure, NOT read as the all-clear (gt-a7um) --------
#
# `git rev-list origin/main..HEAD` exits non-zero with empty stdout when
# `origin/main` doesn't resolve at all — before the fix, that empty stdout
# was indistinguishable from a genuine "nothing ahead" and the whole push
# block (including any warning) was skipped silently.

log "=== Scenario: no origin remote configured, work stranded (gt-a7um) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
# Seed content differs from the dolt mock's exported "{"id":"1"}" (default),
# so run.sh's own commit step creates a genuine new local commit this cycle —
# one that can never be verified against origin/main once it's gone.
git -C "$SANDBOX/home/gt/.dolt-archive/git" remote remove origin

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_escalated "$SANDBOX" "git backup add/commit/push failed" "no origin remote — must not read as all-clear"
assert_fingerprint "$SANDBOX" "git backup add/commit/push failed" "dolt-archive:git-backup-failed" "no origin remote"
assert_output_contains "$SANDBOX" "git=failed" "no origin remote — git clause"
if grep -qF "git=nothing-to-push" "$SANDBOX/output.log" 2>/dev/null; then
  echo "FAIL: no origin remote — output.log still contains the misleading git=nothing-to-push"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

# --- Scenario 11: origin remote absent but a stale refs/remotes/origin/main --
# --- ref survives, with an unpushed commit — same failure, different half ---
# --- of the same defect (gt-a7um round-5 finding: "or origin/main is missing")

log "=== Scenario: origin removed, stale tracking ref remains, work stranded (gt-a7um) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
REPO="$SANDBOX/home/gt/.dolt-archive/git"
STALE_SHA="$(git -C "$REPO" rev-parse HEAD)"
git -C "$REPO" remote remove origin
git -C "$REPO" update-ref refs/remotes/origin/main "$STALE_SHA"

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_escalated "$SANDBOX" "git backup add/commit/push failed" "stale tracking ref, no remote — must not read as all-clear"
assert_fingerprint "$SANDBOX" "git backup add/commit/push failed" "dolt-archive:git-backup-failed" "stale tracking ref, no remote"
assert_output_contains "$SANDBOX" "git=failed" "stale tracking ref, no remote — git clause"
if grep -qF "git=nothing-to-push" "$SANDBOX/output.log" 2>/dev/null; then
  echo "FAIL: stale tracking ref, no remote — output.log still contains the misleading git=nothing-to-push"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "Suite A (escalation logic): FAILED — $FAILURES scenario(s) failed"
else
  echo "Suite A (escalation logic): PASSED — all scenarios passed"
fi

# =============================================================================
# Suite B: Dolt-push visibility guard (gt-v3df)
#
# Runs the REAL run.sh (not a copy) end-to-end against a scratch DOLT_DATA_DIR
# with fake `dolt`/`gh`/`bd`/`gt` binaries shimmed onto PATH. Nothing here
# touches a real remote, real Dolt server, or the production databases — per
# gt-v3df's instruction to use a scratch repo and never attempt a real push
# while testing.
# =============================================================================

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

FAKE_BIN="$WORKDIR/bin"
DOLT_DATA_DIR="$WORKDIR/dolt-data"
PUSH_LOG="$WORKDIR/push.log"
ESCALATE_LOG="$WORKDIR/escalate.log"
mkdir -p "$FAKE_BIN"
touch "$PUSH_LOG" "$ESCALATE_LOG"

# --- Fake dolt: no real Dolt server or push is ever contacted ---------------
cat > "$FAKE_BIN/dolt" <<'FAKE_DOLT'
#!/usr/bin/env bash
set -euo pipefail
args=("$@")
joined=" ${args[*]} "

if [[ "$1" == "remote" && "${2:-}" == "-v" ]]; then
  [[ -f ".remotes" ]] && cat ".remotes"
  exit 0
fi

if [[ "$1" == "push" ]]; then
  echo "$(pwd)|${2:-}" >> "$PUSH_LOG"
  exit 0
fi

if [[ "$joined" == *" sql -q "* ]]; then
  # Every SQL query in this test returns a header-only CSV/JSON result — run.sh
  # reads this as "no issues table" and moves on without touching real data.
  echo "Tables_in_db"
  exit 0
fi

exit 0
FAKE_DOLT
chmod +x "$FAKE_BIN/dolt"

# --- Fake gh: visibility keyed off the owner segment, no network ------------
cat > "$FAKE_BIN/gh" <<'FAKE_GH'
#!/usr/bin/env bash
if [[ "$1" == "api" ]]; then
  repo=""
  for a in "$@"; do
    if [[ "$a" == repos/* ]]; then
      repo="${a#repos/}"
    fi
  done
  owner="${repo%%/*}"
  case "$owner" in
    pub-owner)  echo "public" ;;
    priv-owner) echo "private" ;;
    *) echo "public" ;;
  esac
  exit 0
fi
exit 1
FAKE_GH
chmod +x "$FAKE_BIN/gh"

# --- Fake bd/gt: record escalations, never touch real beads/Dolt ------------
cat > "$FAKE_BIN/bd" <<'FAKE_BD'
#!/usr/bin/env bash
[[ "$1" == "create" ]] && echo "fake-bd-id"
exit 0
FAKE_BD
chmod +x "$FAKE_BIN/bd"

cat > "$FAKE_BIN/gt" <<FAKE_GT
#!/usr/bin/env bash
if [[ "\$1" == "escalate" ]]; then
  echo "\$*" >> "$ESCALATE_LOG"
fi
exit 0
FAKE_GT
chmod +x "$FAKE_BIN/gt"

# --- Scratch databases --------------------------------------------------

# db-public: remote resolves to a public GitHub repo -> push MUST be refused.
mkdir -p "$DOLT_DATA_DIR/db-public/.dolt"
echo "origin git+https://github.com/pub-owner/repo {}" > "$DOLT_DATA_DIR/db-public/.remotes"

# db-private: remote resolves to a private GitHub repo -> push proceeds.
mkdir -p "$DOLT_DATA_DIR/db-private/.dolt"
echo "origin git+https://github.com/priv-owner/repo {}" > "$DOLT_DATA_DIR/db-private/.remotes"

# run.sh's JSONL_EXPORT_DIR is hardcoded (not env-overridable), so the prune
# loop's `ls -t "$JSONL_EXPORT_DIR/${DB}-2"*.jsonl` globs against the real
# directory. With zero prior snapshots for a never-before-seen DB name the
# glob is left literal, `ls` exits 1, and under `set -o pipefail` that fails
# the bare `SNAPSHOTS=$(...)` assignment — an unrelated pre-existing bug
# (crashes any first-ever run for a new DB, silently, before this guard's
# code even runs) that a real db-public/db-private DB would also hit. Touch
# a placeholder snapshot so the glob matches and the unrelated bug doesn't
# mask this test; remove it again on exit either way.
REAL_JSONL_DIR="$HOME/gt/.dolt-archive/jsonl"
mkdir -p "$REAL_JSONL_DIR"
touch "$REAL_JSONL_DIR/db-public-20000101-0000.jsonl" "$REAL_JSONL_DIR/db-private-20000101-0000.jsonl"
trap 'rm -rf "$WORKDIR"; rm -f "$REAL_JSONL_DIR/db-public-20000101-0000.jsonl" "$REAL_JSONL_DIR/db-private-20000101-0000.jsonl"' EXIT

OUTPUT="$WORKDIR/run.log"
set +e
PATH="$FAKE_BIN:$PATH" \
  DOLT_DATA_DIR="$DOLT_DATA_DIR" \
  PUSH_LOG="$PUSH_LOG" \
  ESCALATE_LOG="$ESCALATE_LOG" \
  bash "$RUN_SH" --databases db-public,db-private --skip-git > "$OUTPUT" 2>&1
RUN_STATUS=$?
set -e

PASS=0
FAIL=0

check() {
  local desc="$1" cond="$2"
  if eval "$cond"; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "FAIL: $desc"
  fi
}

check "run.sh exited 0" '[[ "$RUN_STATUS" -eq 0 ]]'
check "db-public was NOT pushed" '! grep -q "/db-public|origin" "$PUSH_LOG"'
check "db-private WAS pushed" 'grep -q "/db-private|origin" "$PUSH_LOG"'
check "refusal is escalated with a fingerprint" 'grep -q "push-refused:db-public:origin" "$ESCALATE_LOG"'
check "no escalation for the private (allowed) push" '! grep -q "db-private:origin" "$ESCALATE_LOG"'
check "summary line reports the refusal, not a silent skip" 'grep -q "dolt_push_refused=1" "$OUTPUT"'
check "log shows the refusal reason inline" 'grep -q "REFUSED.*visibility=public" "$OUTPUT"'

echo ""
echo "Suite B (visibility guard): $PASS passed, $FAIL failed"
if [[ "$FAIL" -ne 0 ]]; then
  echo "--- run.sh output ---"
  cat "$OUTPUT"
fi

# --- Combined result ---------------------------------------------------------

echo ""
if [[ "$FAILURES" -gt 0 || "$FAIL" -gt 0 ]]; then
  echo "FAILED: Suite A had $FAILURES failure(s), Suite B had $FAIL failure(s)"
  exit 1
fi
echo "PASSED: all scenarios in both suites passed"
