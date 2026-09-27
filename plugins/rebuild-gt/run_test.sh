#!/usr/bin/env bash
# Tests for rebuild-gt/run.sh building from a dedicated worktree.
#
# Regression test for: run.sh built from $TOWN_ROOT/gastown/mayor/rig (the
# mayor's working clone) and skipped whenever that clone was dirty or off
# main. The mayor's clone routinely carries beads-state edits and a local
# branch, so the rebuild silently skipped for hours (gt-f612).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0

WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "$WORK_ROOT"' EXIT

# --- Fixture: a bare "origin" and a dirty, off-main mayor clone -------------

ORIGIN_DIR="$WORK_ROOT/origin.git"
git init --bare -q "$ORIGIN_DIR"

TOWN_ROOT="$WORK_ROOT/townroot"
RIG_ROOT="$TOWN_ROOT/gastown/mayor/rig"
BUILD_ROOT="$TOWN_ROOT/gastown/.gt-build"
SKIP_COUNT_FILE="$TOWN_ROOT/gastown/.gt-build-skip-count"
mkdir -p "$(dirname "$RIG_ROOT")"

git clone -q "$ORIGIN_DIR" "$RIG_ROOT"
git -C "$RIG_ROOT" config user.email "test@example.com"
git -C "$RIG_ROOT" config user.name "Test"
git -C "$RIG_ROOT" commit --allow-empty -q -m "initial"
git -C "$RIG_ROOT" branch -M main
git -C "$RIG_ROOT" push -q origin main

# Mayor's clone: dirty, and parked on a feature branch — exactly the state
# that used to make rebuild-gt skip forever.
git -C "$RIG_ROOT" checkout -q -b some-feature
echo "wip" >> "$RIG_ROOT/scratch.txt"

ORIGIN_MAIN_SHA="$(git -C "$ORIGIN_DIR" rev-parse main)"

# --- Stub gt/bd/make on PATH --------------------------------------------------

STUB_BIN="$WORK_ROOT/bin"
mkdir -p "$STUB_BIN"
MAKE_LOG="$WORK_ROOT/make_calls.log"
ESCALATE_LOG="$WORK_ROOT/escalate_calls.log"
: > "$MAKE_LOG"
: > "$ESCALATE_LOG"

STALE_JSON_FILE="$WORK_ROOT/stale.json"

cat > "$STUB_BIN/gt" <<EOF
#!/usr/bin/env bash
if [ "\$1" = "town" ] && [ "\$2" = "root" ]; then
  echo "$TOWN_ROOT"
  exit 0
fi
if [ "\$1" = "stale" ]; then
  cat "$STALE_JSON_FILE"
  exit 0
fi
if [ "\$1" = "version" ]; then
  echo "gt version test-fake"
  exit 0
fi
if [ "\$1" = "escalate" ]; then
  echo "\$@" >> "$ESCALATE_LOG"
  exit 0
fi
exit 1
EOF
chmod +x "$STUB_BIN/gt"

cat > "$STUB_BIN/bd" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "create" ]; then
  echo "fake-receipt-id"
  exit 0
fi
exit 0
EOF
chmod +x "$STUB_BIN/bd"

cat > "$STUB_BIN/make" <<EOF
#!/usr/bin/env bash
echo "cwd=\$(pwd) args=\$*" >> "$MAKE_LOG"
exit 0
EOF
chmod +x "$STUB_BIN/make"

run_rebuild() {
  PATH="$STUB_BIN:$PATH" GT_TOWN_ROOT="$TOWN_ROOT" bash "$SCRIPT_DIR/run.sh"
}

# --- Test 1: dirty/off-main mayor clone still rebuilds from origin/main -----

cat > "$STALE_JSON_FILE" <<'EOF'
{"stale": true, "forward": true, "on_main_branch": false, "safe_to_rebuild": false}
EOF
: > "$MAKE_LOG"
rm -f "$SKIP_COUNT_FILE"

OUTPUT="$(run_rebuild 2>&1)" || { echo "FAIL: forward+stale run exited non-zero: $OUTPUT"; FAILURES=$((FAILURES + 1)); }

if ! grep -q "cwd=$BUILD_ROOT" "$MAKE_LOG"; then
  echo "FAIL: make was not invoked from the dedicated build worktree ($BUILD_ROOT)"
  echo "  make log: $(cat "$MAKE_LOG")"
  FAILURES=$((FAILURES + 1))
fi

if grep -q "cwd=$RIG_ROOT" "$MAKE_LOG"; then
  echo "FAIL: make was invoked from the mayor's clone ($RIG_ROOT) — must never build there"
  FAILURES=$((FAILURES + 1))
fi

BUILD_SHA="$(git -C "$BUILD_ROOT" rev-parse HEAD 2>/dev/null || echo "MISSING")"
if [ "$BUILD_SHA" != "$ORIGIN_MAIN_SHA" ]; then
  echo "FAIL: build worktree HEAD ($BUILD_SHA) != origin/main ($ORIGIN_MAIN_SHA)"
  FAILURES=$((FAILURES + 1))
fi

RIG_BRANCH_AFTER="$(git -C "$RIG_ROOT" branch --show-current)"
if [ "$RIG_BRANCH_AFTER" != "some-feature" ]; then
  echo "FAIL: mayor's clone branch was touched (now: $RIG_BRANCH_AFTER)"
  FAILURES=$((FAILURES + 1))
fi

if [ -z "$(git -C "$RIG_ROOT" status --porcelain)" ]; then
  echo "FAIL: mayor's clone is no longer dirty — it should never be touched"
  FAILURES=$((FAILURES + 1))
fi

# --- Test 2: a downgrade (forward=false) is refused, never calls make ------

cat > "$STALE_JSON_FILE" <<'EOF'
{"stale": true, "forward": false, "on_main_branch": false, "safe_to_rebuild": false}
EOF
: > "$MAKE_LOG"
rm -f "$SKIP_COUNT_FILE"

run_rebuild >/dev/null 2>&1 || { echo "FAIL: downgrade run exited non-zero (should skip cleanly)"; FAILURES=$((FAILURES + 1)); }

if [ -s "$MAKE_LOG" ]; then
  echo "FAIL: make was invoked despite forward=false (downgrade must be refused)"
  FAILURES=$((FAILURES + 1))
fi

# --- Test 3: escalate after 3 consecutive stale-but-skipped runs ------------

rm -f "$SKIP_COUNT_FILE"
: > "$ESCALATE_LOG"

run_rebuild >/dev/null 2>&1 || true
run_rebuild >/dev/null 2>&1 || true
if [ -s "$ESCALATE_LOG" ]; then
  echo "FAIL: escalated before reaching the skip threshold"
  FAILURES=$((FAILURES + 1))
fi

run_rebuild >/dev/null 2>&1 || true
if ! [ -s "$ESCALATE_LOG" ]; then
  echo "FAIL: did not escalate after 3 consecutive skipped-while-stale runs"
  FAILURES=$((FAILURES + 1))
fi

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES test(s) failed"
  exit 1
else
  echo "PASSED: all tests passed"
fi
