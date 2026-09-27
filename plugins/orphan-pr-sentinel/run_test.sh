#!/usr/bin/env bash
# Tests for orphan-pr-sentinel/run.sh's ownership detection.
#
# Regression test for gt-gy1b: a PR that has finished (work bead closed at
# `gt done`) but is still queued has an OPEN merge-request bead, which lives in
# the wisps table and cites the PR via `review_pr:` / `branch:` rather than
# "PR #<N>". The sentinel only searched titles/descriptions of ordinary beads,
# so every such PR was reported as "no owning bead", once per dog per cycle.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0

WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "$WORK_ROOT"' EXIT

# Fake town: one rig whose refinery/rig checkout has a github.com origin.
TOWN="$WORK_ROOT/town"
mkdir -p "$TOWN/mayor" "$TOWN/testrig/refinery"
echo '{"rigs": {"testrig": {}}}' > "$TOWN/mayor/rigs.json"
git init -q "$TOWN/testrig/refinery/rig"
git -C "$TOWN/testrig/refinery/rig" remote add origin "https://github.com/test-org/test-repo.git"

STUB_BIN="$WORK_ROOT/bin"
mkdir -p "$STUB_BIN"

# Open PRs. Every PR has one unresolved thread (see the gt stub), so the only
# thing standing between a PR and a NO-BEAD finding is ownership detection.
#   501 - no bead, no MR                              -> must be reported
#   502 - closed work bead + open MR (review_pr: 502) -> owned
#   503 - open MR matching only by branch             -> owned
#   504 - only a CLOSED MR (review_pr: 504)           -> must be reported
cat > "$WORK_ROOT/prs.json" <<'JSON'
[
  {"number": 501, "title": "unowned", "statusCheckRollup": [], "headRefOid": "sha501", "headRefName": "polecat/a/gt-501@x"},
  {"number": 502, "title": "queued via review_pr", "statusCheckRollup": [], "headRefOid": "sha502", "headRefName": "polecat/b/gt-502@x"},
  {"number": 503, "title": "queued via branch", "statusCheckRollup": [], "headRefOid": "sha503", "headRefName": "polecat/c/gt-503@x"},
  {"number": 504, "title": "only a closed MR", "statusCheckRollup": [], "headRefOid": "sha504", "headRefName": "polecat/d/gt-504@x"}
]
JSON

# MR wisp rows as `bd sql --json` returns them. 5010 is a number-boundary
# decoy: it must not own PR 501.
cat > "$WORK_ROOT/mrs.json" <<'JSON'
[
  {"id": "gt-wisp-a", "status": "open",    "description": "branch: polecat/b/gt-502@x\ntarget: main\nreview_pr: 502\nreview_loop_iter: 1"},
  {"id": "gt-wisp-b", "status": "blocked", "description": "branch: polecat/c/gt-503@x\ntarget: main\nsource_issue: gt-503"},
  {"id": "gt-wisp-c", "status": "closed",  "description": "branch: polecat/d/gt-504@x\ntarget: main\nreview_pr: 504"},
  {"id": "gt-wisp-d", "status": "open",    "description": "branch: polecat/z/gt-5010@x\ntarget: main\nreview_pr: 5010"}
]
JSON

cat > "$STUB_BIN/gh" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "pr list") cat "$WORK_ROOT/prs.json" ;;
  "api repos/"*) echo MEMBER ;;
  *) exit 1 ;;
esac
STUB

cat > "$STUB_BIN/gt" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = "refinery" ] && [ "$2" = "pr" ] && [ "$3" = "threads" ]; then
  echo '[{"id": "t1"}]'
  exit 0
fi
exit 0
STUB

# bd: no ordinary bead references any PR (work beads are closed); MR wisps come
# from `bd sql --json`. Like the real bd, warnings go to stderr and a failed sql
# query prints its error on stdout.
cat > "$STUB_BIN/bd" <<'STUB'
#!/usr/bin/env bash
echo "Warning: stub" >&2
for a in "$@"; do
  case "$a" in
    list) echo '[]'; exit 0 ;;
    sql)
      if [ -n "${STUB_SQL_FAIL:-}" ]; then
        echo 'Error 1146: table not found: wisps'
        exit 1
      fi
      cat "$WORK_ROOT/mrs.json"
      exit 0 ;;
  esac
done
exit 0
STUB
chmod +x "$STUB_BIN"/*

run_sentinel() {
  PATH="$STUB_BIN:$PATH" WORK_ROOT="$WORK_ROOT" GT_TOWN_ROOT="$TOWN" \
    GT_ORPHAN_PR_SENTINEL_STATE="$WORK_ROOT/state.json" DRY_RUN=1 \
    bash "$SCRIPT_DIR/run.sh" 2>&1
}

assert_flagged() {
  local out="$1" pr="$2" why="$3"
  if grep -q "NO-BEAD: testrig PR #$pr " <<< "$out"; then
    echo "PASS: PR #$pr reported ($why)"
  else
    echo "FAIL: PR #$pr should be reported ($why)"; FAILURES=$((FAILURES + 1))
  fi
}

assert_not_flagged() {
  local out="$1" pr="$2" why="$3"
  if grep -q "NO-BEAD: testrig PR #$pr " <<< "$out"; then
    echo "FAIL: PR #$pr must not be reported ($why)"; FAILURES=$((FAILURES + 1))
  else
    echo "PASS: PR #$pr not reported ($why)"
  fi
}

OUT="$(run_sentinel)"

# Positive controls: without these a negative pass could just mean the script
# never reached the ownership check.
assert_flagged     "$OUT" 501 "no bead and no MR; 5010 is not a prefix match"
assert_flagged     "$OUT" 504 "a closed MR does not own the PR"
assert_not_flagged "$OUT" 502 "open MR with review_pr"
assert_not_flagged "$OUT" 503 "open MR matching only the PR head branch"

# Query failure is unknown ownership, not zero owners: no finding, no "Clean".
OUT="$(STUB_SQL_FAIL=1 run_sentinel)"
for pr in 501 502 503 504; do
  assert_not_flagged "$OUT" "$pr" "MR query failed, ownership unknown"
done
if grep -q "ERROR: .*merge-request" <<< "$OUT"; then
  echo "PASS: MR query failure is logged"
else
  echo "FAIL: MR query failure should be logged as an ERROR"; FAILURES=$((FAILURES + 1))
fi

if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES test(s) failed"
  exit 1
fi
echo "All tests passed"
