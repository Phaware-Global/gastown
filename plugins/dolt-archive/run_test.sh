#!/usr/bin/env bash
# run_test.sh — integration tests for dolt-archive/run.sh.
#
# Three independent suites, run back-to-back, neither replacing the other:
#   Suite A (gt-igzm): escalation logic — mocked dolt/bd/gt, asserts on which
#     escalations fire (dolt push failure, missing git backup repo, remote
#     shortfall, git push failure/success/nothing-to-push, fingerprint dedup).
#   Suite B (gt-v3df): the Dolt-push visibility guard — runs the REAL run.sh
#     against a scratch DOLT_DATA_DIR with fake dolt/gh/bd/gt binaries,
#     asserting a public remote is refused and a private one is pushed.
#
#   Suite C (gt-sg6n): the same guard on the JSONL git push layer — public,
#     non-GitHub and undeterminable origins are refused; a private one is pushed.
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
#  12-14. a failed export (or failed table check) must not leave the previous
#     cycle's -latest.jsonl behind, and the git step must not commit that old
#     data as this cycle's snapshot (gt-pgvd)
#  15+. run.sh's own once-per-condition escalation dedupe — identical/changed/
#     cleared-then-returned/skip-scoped/interval-digest/retried-after-failure
#     conditions, tracked per independently-resolvable entity (a db, or a
#     db+remote pair) with no shared history between entities, so one
#     member's resolve-then-return can never be masked by an unrelated
#     member's ongoing failure (gt-4kip, mayor ruling on #250 / hq-cv-6yo5w)
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
    if [[ -f "${DOLT_DATA_DIR:-$HOME/gt/.dolt-data}/$db/.mock-table-check-fail" ]]; then
      echo "mock table check failure for $db" >&2
      exit 1
    fi
    printf 'Tables_in_db\n'
    if [[ ! -f "${DOLT_DATA_DIR:-$HOME/gt/.dolt-data}/$db/.mock-no-issues-table" ]]; then
      printf 'issues\n'
    fi
    ;;
  "SELECT * FROM issues ORDER BY id")
    if [[ -f "${DOLT_DATA_DIR:-$HOME/gt/.dolt-data}/$db/.mock-export-fail" ]]; then
      echo "mock export failure for $db" >&2
      exit 1
    fi
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
  if [[ -n "\$fp" && ! -f "$sandbox/.no-server-dedup" ]]; then
    mkdir -p "$sandbox/.fingerprints"
    seen_file="$sandbox/.fingerprints/\${fp//[^A-Za-z0-9_.-]/_}"
    if [[ -f "\$seen_file" ]]; then
      exit 0
    fi
    touch "\$seen_file"
  fi
  if [[ -f "$sandbox/.gt-escalate-fails" ]]; then
    echo "mock gt escalate failure" >&2
    exit 1
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

# The git-layer visibility guard (gt-sg6n) judges the URL git would actually
# push to (`git remote get-url --push --all origin`). These scenarios need a
# REAL push to a local bare repo to succeed or fail, while the guard sees a
# github.com URL — so the git shim below answers `git remote get-url` from
# $sandbox/git-url-override when that file exists, and otherwise passes every
# call straight through to the real git. Only the URL the guard reads is
# faked; add/commit/rev-list/push all run for real.
REAL_GIT="$(command -v git)"
write_git_shim() {
  local sandbox="$1" override_url="${2:-}"
  cat > "$sandbox/bin/git" <<MOCK
#!/usr/bin/env bash
if [[ "\$1" == "remote" && "\${2:-}" == "get-url" && -f "$sandbox/git-url-override" ]]; then
  cat "$sandbox/git-url-override"
  exit 0
fi
exec "$REAL_GIT" "\$@"
MOCK
  chmod +x "$sandbox/bin/git"
  if [[ -n "$override_url" ]]; then
    printf '%s\n' "$override_url" > "$sandbox/git-url-override"
  fi
}

# gh mock keyed off the owner segment, for the git-layer guard scenarios.
write_gh_owner_mock() {
  local sandbox="$1"
  cat > "$sandbox/bin/gh" <<'MOCK'
#!/usr/bin/env bash
if [[ "$1" == "api" ]]; then
  repo=""
  for a in "$@"; do [[ "$a" == repos/* ]] && repo="${a#repos/}"; done
  case "${repo%%/*}" in
    priv-owner) echo "private" ;;
    lookup-fails-owner) exit 1 ;;
    *) echo "public" ;;
  esac
  exit 0
fi
exit 1
MOCK
  chmod +x "$sandbox/bin/gh"
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
if grep -qF "git_push_refused" "$SANDBOX/output.log" 2>/dev/null; then
  echo "FAIL: missing git backup repo — summary reports git_push_refused although the guard never ran"
  FAILURES=$((FAILURES + 1))
fi
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
write_gh_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
write_git_shim "$SANDBOX" "https://github.com/test-owner/example-backup"

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
write_gh_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
write_git_shim "$SANDBOX" "https://github.com/test-owner/example-backup"

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

# --- Scenarios 12-14: a failed export must not republish an older snapshot ---
# --- as the current one (gt-pgvd) --------------------------------------------
#
# A previous cycle left testdb-latest.jsonl -> testdb-<old>.jsonl (content
# "yesterday"). The git backup repo holds still-older content ("older"). If the
# export fails and the stale link survives, the git step copies "yesterday"
# into the repo and commits it as "Archive snapshot <now>" — old data under
# today's name, with nothing in the repo history to tell them apart.

# Seeds yesterday's snapshot + -latest link for $2 (db name) in sandbox $1.
seed_stale_latest() {
  local sandbox="$1" db="$2"
  local dir="$sandbox/home/gt/.dolt-archive/jsonl"
  printf '{"id":"yesterday"}\n' > "$dir/${db}-20000101-0000.jsonl"
  ln -s "${db}-20000101-0000.jsonl" "$dir/${db}-latest.jsonl"
}

# Asserts no commit in the backup repo ever carried $2 as testdb.jsonl content
# beyond the seed, i.e. the stale data was not committed under a new snapshot.
assert_no_stale_commit() {
  local sandbox="$1" desc="$2"
  local repo="$sandbox/home/gt/.dolt-archive/git"
  local commits
  commits="$(git -C "$repo" rev-list --count HEAD)"
  if [[ "$commits" -ne 1 ]]; then
    echo "FAIL: $desc — expected the backup repo to stay at its seed commit, got $commits commits:"
    git -C "$repo" log --format='    %h %s' | head -5
    FAILURES=$((FAILURES + 1))
  fi
  if git -C "$repo" grep -q yesterday HEAD -- testdb.jsonl 2>/dev/null; then
    echo "FAIL: $desc — yesterday's data was committed to the backup repo as a new snapshot"
    FAILURES=$((FAILURES + 1))
  fi
}

assert_latest_absent() {
  local sandbox="$1" db="$2" desc="$3"
  local link="$sandbox/home/gt/.dolt-archive/jsonl/${db}-latest.jsonl"
  if [[ -e "$link" || -L "$link" ]]; then
    echo "FAIL: $desc — ${db}-latest.jsonl still present after a failed export -> $(readlink "$link" 2>/dev/null || echo '(file)')"
    FAILURES=$((FAILURES + 1))
  fi
}

log "=== Scenario: failed export must not leave a stale -latest link (gt-pgvd) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX" '{"id":"older"}'
mkdir -p "$SANDBOX/home/gt/.dolt-data/testdb"
touch "$SANDBOX/home/gt/.dolt-data/testdb/.mock-export-fail"
seed_stale_latest "$SANDBOX" testdb

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_latest_absent "$SANDBOX" testdb "failed export"
assert_no_stale_commit "$SANDBOX" "failed export"
assert_output_contains "$SANDBOX" "removed stale testdb-latest.jsonl" "failed export — removal is visible in the run output"
assert_output_contains "$SANDBOX" "jsonl=0/1" "failed export — summary counts the failure"
assert_escalated "$SANDBOX" "JSONL export failed" "failed export"
# The dated snapshot itself is history, not a claim of currency: it stays.
if [[ ! -f "$SANDBOX/home/gt/.dolt-archive/jsonl/testdb-20000101-0000.jsonl" ]]; then
  echo "FAIL: failed export — the dated historical snapshot was deleted"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

log "=== Scenario: failed table check must not leave a stale -latest link (gt-pgvd) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX" '{"id":"older"}'
mkdir -p "$SANDBOX/home/gt/.dolt-data/testdb"
touch "$SANDBOX/home/gt/.dolt-data/testdb/.mock-table-check-fail"
seed_stale_latest "$SANDBOX" testdb

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
assert_latest_absent "$SANDBOX" testdb "failed table check"
assert_no_stale_commit "$SANDBOX" "failed table check"
assert_output_contains "$SANDBOX" "removed stale testdb-latest.jsonl" "failed table check — removal is visible in the run output"
assert_escalated "$SANDBOX" "JSONL export failed" "failed table check"
rm -rf "$SANDBOX"

log "=== Scenario: one db fails, the other exports — only the failed db loses its link (gt-pgvd) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX" '{"id":"older"}'
mkdir -p "$SANDBOX/home/gt/.dolt-data/testdb" "$SANDBOX/home/gt/.dolt-data/gooddb"
touch "$SANDBOX/home/gt/.dolt-data/testdb/.mock-export-fail"
seed_stale_latest "$SANDBOX" testdb
seed_stale_latest "$SANDBOX" gooddb

run_scenario "$SANDBOX" --databases testdb,gooddb --skip-dolt-push
assert_latest_absent "$SANDBOX" testdb "failed db in a mixed run"
GOOD_LINK="$SANDBOX/home/gt/.dolt-archive/jsonl/gooddb-latest.jsonl"
if [[ ! -L "$GOOD_LINK" ]] || [[ "$(readlink "$GOOD_LINK")" == "gooddb-20000101-0000.jsonl" ]]; then
  echo "FAIL: mixed run — gooddb-latest.jsonl was not advanced to this cycle's fresh export"
  FAILURES=$((FAILURES + 1))
fi
REPO="$SANDBOX/home/gt/.dolt-archive/git"
if git -C "$REPO" grep -q yesterday HEAD 2>/dev/null; then
  echo "FAIL: mixed run — yesterday's data reached the backup repo"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

log "=== Scenario: stale link cannot be removed — cycle continues, escalates, stale data not committed (gt-pgvd) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"
write_bd_mock "$SANDBOX"
write_gt_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX" '{"id":"older"}'
mkdir -p "$SANDBOX/home/gt/.dolt-data/testdb"
touch "$SANDBOX/home/gt/.dolt-data/testdb/.mock-export-fail"
seed_stale_latest "$SANDBOX" testdb
# Read-only jsonl dir: the export write and the link removal both get EACCES.
chmod a-w "$SANDBOX/home/gt/.dolt-archive/jsonl"

run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
chmod u+w "$SANDBOX/home/gt/.dolt-archive/jsonl"
assert_output_contains "$SANDBOX" "could not remove stale testdb-latest.jsonl" "unremovable stale link — failure is visible in the run output"
assert_output_contains "$SANDBOX" "jsonl=0/1" "unremovable stale link — cycle reached the summary"
assert_escalated "$SANDBOX" "JSONL export failed" "unremovable stale link"
assert_no_stale_commit "$SANDBOX" "unremovable stale link"
rm -rf "$SANDBOX"

# --- Scenarios 15+: run.sh's OWN once-per-condition dedupe (gt-4kip) --------
#
# `gt escalate --fingerprint` only suppresses against OPEN escalation beads,
# so once the mayor closes a repeat the next cycle files it again — alert
# fatigue. These scenarios switch the mock's server-side dedupe OFF (as if
# every prior escalation had already been closed) so only run.sh's own
# state can keep a static condition from re-escalating.

# Runs one archive cycle WITHOUT truncating escalate.log (run_scenario does).
run_cycle() {
  local sandbox="$1"; shift
  (
    export HOME="$sandbox/home"
    export PATH="$sandbox/bin:$PATH"
    export DOLT_DATA_DIR="$sandbox/home/gt/.dolt-data"
    "$RUN_SH" "$@"
  ) >> "$sandbox/output.log" 2>&1 || true
}

count_escalations() {
  local sandbox="$1" needle="$2" n
  n="$(grep -c -- "$needle" "$sandbox/escalate.log" 2>/dev/null || true)"
  echo "${n:-0}"
}

assert_count() {
  local sandbox="$1" needle="$2" want="$3" desc="$4" got
  got="$(count_escalations "$sandbox" "$needle")"
  if [[ "$got" -ne "$want" ]]; then
    echo "FAIL: $desc — expected $want escalation(s) matching '$needle', got $got"
    echo "  escalate.log contents:"
    sed 's/^/    /' "$sandbox/escalate.log" 2>/dev/null || echo "    (empty)"
    FAILURES=$((FAILURES + 1))
  fi
}

# Sandbox with three static conditions at once: a failing dolt push, no git
# backup repo, and an exported db with no remote (the shape from gt-4kip).
setup_static_conditions() {
  local sandbox
  sandbox="$(setup_sandbox)"
  write_dolt_mock "$sandbox"
  write_bd_mock "$sandbox"
  write_gt_mock "$sandbox"
  write_gh_mock "$sandbox"
  touch "$sandbox/.no-server-dedup"
  mkdir -p "$sandbox/home/gt/.dolt-data/db-a/.dolt" "$sandbox/home/gt/.dolt-data/db-b/.dolt"
  printf 'origin\thttps://github.com/test-owner/db-a (fetch)\n' > "$sandbox/home/gt/.dolt-data/db-a/.mock-remotes"
  touch "$sandbox/home/gt/.dolt-data/db-a/.mock-push-fail-origin"
  echo "$sandbox"
}

log "=== Scenario: identical state across two runs escalates once (gt-4kip) ==="
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
run_cycle "$SANDBOX" --databases db-a,db-b
run_cycle "$SANDBOX" --databases db-a,db-b
assert_count "$SANDBOX" "dolt push failed" 1 "identical state, 2 runs — push failure"
assert_count "$SANDBOX" "no git backup repo" 1 "identical state, 2 runs — missing git repo"
assert_count "$SANDBOX" "exported databases have a dolt remote" 1 "identical state, 2 runs — remote shortfall"
assert_count "$SANDBOX" "dolt-archive:" 3 "identical state, 2 runs — total escalations"
assert_output_contains "$SANDBOX" "unchanged, already escalated" "suppressed repeats are still logged"
rm -rf "$SANDBOX"

log "=== Scenario: changed affected-set re-escalates (gt-4kip) ==="
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
# A second remote on db-a now fails too: the failing set changed.
printf 'origin\thttps://github.com/test-owner/db-a (fetch)\nmirror\thttps://github.com/test-owner/db-a-mirror (fetch)\n' \
  > "$SANDBOX/home/gt/.dolt-data/db-a/.mock-remotes"
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 2 "affected remote set changed — must re-escalate"
if grep -- "dolt push failed" "$SANDBOX/escalate.log" | grep -qv -- "-s critical"; then
  echo "FAIL: changed affected-set — re-escalation must stay critical"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

log "=== Scenario: condition clears then returns re-escalates (gt-4kip) ==="
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
rm -f "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-origin"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-origin"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 2 "cleared then returned — must re-escalate"
# The shortfall never cleared, so it must NOT have re-fired across those 3 runs.
assert_count "$SANDBOX" "exported databases have a dolt remote" 1 "uncleared shortfall stays quiet"
rm -rf "$SANDBOX"

log "=== Scenario: --skip flags do not clear another step's state (gt-4kip) ==="
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
run_cycle "$SANDBOX" --databases db-a,db-b --skip-dolt-push --skip-git
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 1 "skipped dolt push must not read as 'condition cleared'"
rm -rf "$SANDBOX"

log "=== Scenario: long-interval digest is LOW, not CRITICAL (gt-4kip) ==="
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
ESCALATION_REPEAT_SECS=0 run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 2 "interval elapsed — digest fires"
if [[ "$(grep -- "dolt push failed" "$SANDBOX/escalate.log" | grep -c -- "-s low")" -ne 1 ]]; then
  echo "FAIL: interval digest — expected exactly one '-s low' repeat"
  sed 's/^/    /' "$SANDBOX/escalate.log"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

log "=== Scenario: failed escalate is retried, not recorded as sent (gt-4kip) ==="
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
touch "$SANDBOX/.gt-escalate-fails"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
rm -f "$SANDBOX/.gt-escalate-fails"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 1 "escalate that failed once must still be delivered next run"
rm -rf "$SANDBOX"

log "=== Scenario: a resolved member re-escalates when it returns, even ==="
log "=== alongside an unrelated member that never stopped failing (gt-4kip) ==="
# origin+mirror both fail -> escalate (both new). mirror recovers (origin
# keeps failing) -> origin is unchanged (stays quiet); mirror is checked and
# clean, so its own state is dropped. mirror fails again -> it has no prior
# state, so it escalates again — origin's continued, unrelated failure must
# not mask mirror's independent return. This is the mayor's gt-fc13 ruling
# on #250 (hq-cv-6yo5w): a per-condition-class "sticky union" that remembers
# every member it ever saw (so a shrink then regrow looked like "no change")
# is exactly what let a resolved condition go unescalated on return. Per-key
# state with no shared union between members — mirror and origin never touch
# each other's file — makes that impossible.
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
printf 'origin\thttps://github.com/test-owner/db-a (fetch)\nmirror\thttps://github.com/test-owner/db-a-mirror (fetch)\n' \
  > "$SANDBOX/home/gt/.dolt-data/db-a/.mock-remotes"
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
rm -f "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 2 "mirror's return must re-escalate although origin never stopped failing"
rm -rf "$SANDBOX"

log "=== Scenario: a --databases subset run does not clear another db's state (PR #250) ==="
# db-a fails on a full run (escalates). A subsequent run scoped to db-b only
# must not treat db-a's unresolved failure as cleared just because db-a
# wasn't checked this cycle — otherwise the next full run, finding the same
# unfixed db-a failure, would wrongly re-escalate it as "new".
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
run_cycle "$SANDBOX" --databases db-b --skip-git
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 1 "db-b-only run must not clear db-a's unresolved push failure"
rm -rf "$SANDBOX"

log "=== Scenario: a shrunk affected set still sends a digest past REPEAT_SECS (PR #250) ==="
# 2 failing (origin+mirror) -> escalate. Mirror recovers (shrink of the
# escalated set) but REPEAT_SECS has already elapsed -> must send exactly one
# low digest instead of staying silent indefinitely (adversarial finding on
# #250: the shrink branch used to return early with no age check at all).
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
printf 'origin\thttps://github.com/test-owner/db-a (fetch)\nmirror\thttps://github.com/test-owner/db-a-mirror (fetch)\n' \
  > "$SANDBOX/home/gt/.dolt-data/db-a/.mock-remotes"
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
rm -f "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
ESCALATION_REPEAT_SECS=0 run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 2 "shrink past REPEAT_SECS must still send a digest"
if [[ "$(grep -- "dolt push failed" "$SANDBOX/escalate.log" | grep -c -- "-s low")" -ne 1 ]]; then
  echo "FAIL: shrink digest — expected exactly one '-s low' repeat"
  sed 's/^/    /' "$SANDBOX/escalate.log"
  FAILURES=$((FAILURES + 1))
fi
rm -rf "$SANDBOX"

log "=== Scenario: each member's own resolve/return is tracked independently (gt-4kip) ==="
# origin fails -> escalate. mirror joins -> escalate (a new member). mirror
# recovers -> quiet, and mirror's own state is dropped (checked, now clean).
# third joins (mirror still recovered) -> escalate (a new member). mirror
# fails again -> escalate AGAIN: it has no state (dropped when it resolved),
# so its return is indistinguishable from a first-time failure — exactly the
# gt-fc13 requirement. The old "sticky union" design (one shared history per
# condition *class*, remembering every member it ever saw) would have read
# mirror's return here as "already escalated, still in the recorded set" and
# stayed silent — the bug the mayor's ruling on #250 (hq-cv-6yo5w) ended.
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
printf 'origin\thttps://github.com/test-owner/db-a (fetch)\nmirror\thttps://github.com/test-owner/db-a-mirror (fetch)\nthird\thttps://github.com/test-owner/db-a-third (fetch)\n' \
  > "$SANDBOX/home/gt/.dolt-data/db-a/.mock-remotes"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
rm -f "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-third"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
touch "$SANDBOX/home/gt/.dolt-data/db-a/.mock-push-fail-mirror"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 4 "mirror's second return must escalate again, independent of origin/third's own history"
rm -rf "$SANDBOX"

log "=== Scenario: a db's failure recorded on a full run survives an unrelated ==="
log "=== db's --databases subset cycles (val HIGH on #250) ==="
# Full run: db-a fails -> escalate, its own key recorded. A subsequent
# --databases db-b run (db-b starts failing, then recovers) never touches
# db-a's key at all — each db's state lives in its own file, so a run scoped
# to db-b has no way to read or clear db-a's entry. A later full run must
# therefore still find db-a's failure exactly as it left it (unchanged,
# quiet), not re-file it as new.
SANDBOX="$(setup_static_conditions)"
: > "$SANDBOX/escalate.log"
printf 'origin\thttps://github.com/test-owner/db-b (fetch)\n' > "$SANDBOX/home/gt/.dolt-data/db-b/.mock-remotes"
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
touch "$SANDBOX/home/gt/.dolt-data/db-b/.mock-push-fail-origin"
run_cycle "$SANDBOX" --databases db-b --skip-git
rm -f "$SANDBOX/home/gt/.dolt-data/db-b/.mock-push-fail-origin"
run_cycle "$SANDBOX" --databases db-b --skip-git
run_cycle "$SANDBOX" --databases db-a,db-b --skip-git
assert_count "$SANDBOX" "dolt push failed" 2 "db-a's failure must survive unrelated db-b-only subset cycles without re-filing"
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

# Isolate escalation-dedupe state (gt-4kip): without this the suite would read
# and write the real ~/gt/.dolt-archive/escalation-state.
export DOLT_ARCHIVE_STATE_DIR="$WORKDIR/escalation-state"

OUTPUT="$WORKDIR/run.log"
run_suite_b() {
  PATH="$FAKE_BIN:$PATH" \
    DOLT_DATA_DIR="$DOLT_DATA_DIR" \
    PUSH_LOG="$PUSH_LOG" \
    ESCALATE_LOG="$ESCALATE_LOG" \
    bash "$RUN_SH" --databases db-public,db-private --skip-git
}
set +e
run_suite_b > "$OUTPUT" 2>&1
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

# gt-4kip: an unchanged refusal must not re-escalate; a changed remote URL must
# escalate again immediately.
refusals() { grep -c "push-refused:db-public:origin" "$ESCALATE_LOG" || true; }
check "first refusal escalated exactly once" '[[ "$(refusals)" -eq 1 ]]'
run_suite_b > "$WORKDIR/run2.log" 2>&1 || true
check "unchanged refusal on the next run does not re-escalate" '[[ "$(refusals)" -eq 1 ]]'
check "unchanged refusal is still logged" 'grep -q "REFUSED.*visibility=public" "$WORKDIR/run2.log"'
echo "origin git+https://github.com/pub-owner/other-repo {}" > "$DOLT_DATA_DIR/db-public/.remotes"
run_suite_b > "$WORKDIR/run3.log" 2>&1 || true
check "changed remote URL re-escalates immediately" '[[ "$(refusals)" -eq 2 ]]'
check "changed-URL escalation is critical" 'grep "push-refused:db-public:origin" "$ESCALATE_LOG" | tail -1 | grep -q -- "-s critical"'
rm -f "$DOLT_DATA_DIR/db-public/.remotes"
run_suite_b > "$WORKDIR/run4.log" 2>&1 || true
check "refusal state cleared once the remote is gone" '[[ -z "$(ls "$DOLT_ARCHIVE_STATE_DIR" 2>/dev/null | grep push-refused)" ]]'

# Required regression test (mayor ruling on #250 / hq-cv-6yo5w, gt-fc13): the
# unsafe-remote refusal fires, the remote is fixed (a clean run — the push
# actually succeeds, not just "skipped"), the refusal returns, and it must
# fire again, not stay silent because it "already escalated once before".
echo "origin git+https://github.com/priv-owner/repo {}" > "$DOLT_DATA_DIR/db-public/.remotes"
run_suite_b > "$WORKDIR/run5.log" 2>&1 || true
check "remote fixed (now private) — push actually succeeds" 'grep -q "/db-public|origin" "$PUSH_LOG"'
check "remote fixed — refusal state cleared" '[[ -z "$(ls "$DOLT_ARCHIVE_STATE_DIR" 2>/dev/null | grep push-refused)" ]]'
echo "origin git+https://github.com/pub-owner/repo {}" > "$DOLT_DATA_DIR/db-public/.remotes"
run_suite_b > "$WORKDIR/run6.log" 2>&1 || true
check "refusal returns after being fixed — re-escalates (gt-4kip)" '[[ "$(refusals)" -eq 3 ]]'
check "returned refusal is critical, not suppressed as a stale repeat" \
  'grep "push-refused:db-public:origin" "$ESCALATE_LOG" | tail -1 | grep -q -- "-s critical"'

# Required regression test: a --databases subset run must not clear (or
# otherwise disturb) a still-unresolved refusal for a db outside that subset.
mkdir -p "$DOLT_DATA_DIR/db-subset/.dolt"
echo "origin git+https://github.com/pub-owner/repo {}" > "$DOLT_DATA_DIR/db-subset/.remotes"
touch "$REAL_JSONL_DIR/db-subset-20000101-0000.jsonl"
run_suite_b_dbs() {
  PATH="$FAKE_BIN:$PATH" DOLT_DATA_DIR="$DOLT_DATA_DIR" \
    bash "$RUN_SH" --databases "$1" --skip-git
}
run_suite_b_dbs "db-subset,db-private" > "$WORKDIR/subset1.log" 2>&1 || true
run_suite_b_dbs "db-private" > "$WORKDIR/subset2.log" 2>&1 || true
run_suite_b_dbs "db-subset,db-private" > "$WORKDIR/subset3.log" 2>&1 || true
check "db-private-only run must not clear or re-fire db-subset's unresolved refusal" \
  '[[ "$(grep -c "push-refused:db-subset:origin" "$ESCALATE_LOG")" -eq 1 ]]'
rm -f "$REAL_JSONL_DIR/db-subset-20000101-0000.jsonl"

echo ""
echo "Suite B (visibility guard): $PASS passed, $FAIL failed"
if [[ "$FAIL" -ne 0 ]]; then
  echo "--- run.sh output ---"
  cat "$OUTPUT"
fi

# Suite B's escalation-dedupe state override must not leak into Suite C: an
# `export` is process-wide for the rest of this script, so without this every
# git_guard_scenario sandbox below would share Suite B's $WORKDIR/escalation-
# state instead of getting its own per-sandbox state dir, letting one
# scenario's git-push-refused escalation dedupe against an unrelated one's.
unset DOLT_ARCHIVE_STATE_DIR

# =============================================================================
# Suite C: git-layer visibility guard (gt-sg6n)
#
# The JSONL git push (`git push origin main`) must clear the same fail-closed
# visibility check as the dolt push: public, non-GitHub or undeterminable all
# REFUSE; only a GitHub repo confirmed private is pushed. Real git, real bare
# origin; only gh/dolt/bd/gt are faked, and the git shim fakes just the URL
# the guard reads (see write_git_shim). All remotes are example.invalid-style
# placeholders — no real backup repo name appears here.
# =============================================================================

C_FAIL=0
c_fail() { echo "FAIL: $*"; C_FAIL=$((C_FAIL + 1)); }

bare_head() { "$REAL_GIT" -C "$1/bare-origin.git" rev-parse main; }

# git_guard_scenario NAME PUSH_URL EXPECT(refused|pushed) [REASON]
git_guard_scenario() {
  local name="$1" url="$2" expect="$3" reason="${4:-}"
  log "=== Scenario: git-layer guard — $name ==="
  local sb before after
  sb="$(setup_sandbox)"
  write_dolt_mock "$sb"; write_bd_mock "$sb"; write_gt_mock "$sb"
  write_gh_owner_mock "$sb"
  write_git_backup_repo "$sb"
  if [[ -n "$url" ]]; then write_git_shim "$sb" "$url"; else write_git_shim "$sb"; fi
  before="$(bare_head "$sb")"

  run_scenario "$sb" --databases testdb --skip-dolt-push
  after="$(bare_head "$sb")"

  if [[ "$expect" == "refused" ]]; then
    [[ "$before" == "$after" ]] || c_fail "$name — origin received a push despite the guard"
    grep -qF "git=refused" "$sb/output.log" || c_fail "$name — summary lacks git=refused"
    grep -qF "git_push_refused=1" "$sb/output.log" || c_fail "$name — summary lacks git_push_refused=1"
    grep -qF "REFUSED" "$sb/output.log" || c_fail "$name — log lacks REFUSED"
    grep -qF "visibility=$reason" "$sb/output.log" || c_fail "$name — log lacks visibility=$reason"
    grep -q -- "--fingerprint dolt-archive:git-push-refused:origin" "$sb/escalate.log" \
      || c_fail "$name — refusal not escalated with the git-push-refused fingerprint"
    grep -- "git-push-refused" "$sb/escalate.log" | grep -q -- "-s critical" \
      || c_fail "$name — refusal escalation is not -s critical"
    grep -q "result=warning" "$sb/output.log" || c_fail "$name — result is not warning"
    if grep -qF "git=pushed" "$sb/output.log"; then c_fail "$name — reported git=pushed"; fi
  else
    [[ "$before" != "$after" ]] || c_fail "$name — private origin did not receive the push"
    grep -qF "git=pushed" "$sb/output.log" || c_fail "$name — summary lacks git=pushed"
    grep -qF "git_push_refused=0" "$sb/output.log" || c_fail "$name — summary lacks git_push_refused=0"
    assert_no_escalation "$sb" "$name"
  fi
  rm -rf "$sb"
}

git_guard_scenario "public origin is refused"           "https://github.com/pub-owner/example-backup"         refused public
git_guard_scenario "undeterminable visibility refused"  "https://github.com/lookup-fails-owner/example-backup" refused visibility-lookup-failed
git_guard_scenario "non-GitHub origin is refused"       "https://git.example.invalid/team/example-backup.git" refused non-github-remote
git_guard_scenario "private origin is pushed"           "https://github.com/priv-owner/example-backup"        pushed

# A raw local-path origin (no shim, no GitHub URL at all) must also refuse.
log "=== Scenario: git-layer guard — local-path origin is refused ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"; write_bd_mock "$SANDBOX"; write_gt_mock "$SANDBOX"
write_gh_owner_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
BEFORE="$(bare_head "$SANDBOX")"
run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
[[ "$BEFORE" == "$(bare_head "$SANDBOX")" ]] || c_fail "local-path origin received a push"
grep -qF "visibility=non-github-remote" "$SANDBOX/output.log" || c_fail "local-path origin — log lacks visibility=non-github-remote"
rm -rf "$SANDBOX"

# A refusal is per-destination: a second push URL that is public must refuse
# even though the first is private (git pushes to every pushurl).
log "=== Scenario: git-layer guard — any public pushurl refuses ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"; write_bd_mock "$SANDBOX"; write_gt_mock "$SANDBOX"
write_gh_owner_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
write_git_shim "$SANDBOX"
printf '%s\n%s\n' "https://github.com/priv-owner/example-backup" "https://github.com/pub-owner/example-backup" > "$SANDBOX/git-url-override"
BEFORE="$(bare_head "$SANDBOX")"
run_scenario "$SANDBOX" --databases testdb --skip-dolt-push
[[ "$BEFORE" == "$(bare_head "$SANDBOX")" ]] || c_fail "mixed pushurls — origin received a push"
grep -qF "git=refused" "$SANDBOX/output.log" || c_fail "mixed pushurls — summary lacks git=refused"
rm -rf "$SANDBOX"

# Required regression test (val's LOW on #250): a cycle where the git-push
# visibility guard never evaluates at all — not "refused", not "skipped via
# --skip-git", but genuinely never reached (no backup repo present that
# cycle) — must leave a still-unresolved refusal's state untouched. The old
# code cleared git-push-refused:origin whenever `! $SKIP_GIT`, regardless of
# whether the guard itself ever ran that cycle.
log "=== Scenario: git-layer guard — a cycle where the guard never runs leaves a refusal's state untouched (val LOW on #250) ==="
SANDBOX="$(setup_sandbox)"
write_dolt_mock "$SANDBOX"; write_bd_mock "$SANDBOX"; write_gt_mock "$SANDBOX"
write_gh_owner_mock "$SANDBOX"
write_git_backup_repo "$SANDBOX"
write_git_shim "$SANDBOX" "https://github.com/pub-owner/example-backup"
# write_gt_mock's simulated server-side --fingerprint dedup (mirroring real
# `gt escalate`, which only suppresses against OPEN beads) would otherwise
# mask this test: an unchanged refusal always carries the SAME fingerprint,
# so the mock alone would suppress a genuine re-escalation just as readily
# as a correctly-suppressed repeat. Disable it so only run.sh's own
# state-based dedupe is under test, same as the "Scenarios 15+" cases.
touch "$SANDBOX/.no-server-dedup"
: > "$SANDBOX/escalate.log"

run_cycle "$SANDBOX" --databases testdb --skip-dolt-push
REPO="$SANDBOX/home/gt/.dolt-archive/git"
mv "$REPO" "${REPO}.away"
run_cycle "$SANDBOX" --databases testdb --skip-dolt-push
if ! grep -qF "No git backup repo" "$SANDBOX/output.log"; then
  c_fail "guard-never-ran cycle did not actually take the git=missing path"
fi
mv "${REPO}.away" "$REPO"
run_cycle "$SANDBOX" --databases testdb --skip-dolt-push

REFIRES="$(grep -c -- "git-push-refused" "$SANDBOX/escalate.log" || true)"
if [[ "${REFIRES:-0}" -ne 1 ]]; then
  c_fail "guard-never-ran cycle must not clear git-push-refused state — expected 1 total escalation, got ${REFIRES:-0}"
fi
rm -rf "$SANDBOX"

echo ""
echo "Suite C (git-layer guard): $C_FAIL failure(s)"

# --- Combined result ---------------------------------------------------------

echo ""
if [[ "$FAILURES" -gt 0 || "$FAIL" -gt 0 || "$C_FAIL" -gt 0 ]]; then
  echo "FAILED: Suite A had $FAILURES failure(s), Suite B had $FAIL failure(s), Suite C had $C_FAIL failure(s)"
  exit 1
fi
echo "PASSED: all scenarios in both suites passed"
