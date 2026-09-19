#!/usr/bin/env bash
# Tests for git-hygiene/run.sh's --destroy guard on the remote branch DELETE.
#
# Regression test for: report-only (flagless) runs deleted GitHub branches
# because Step 4's `gh api ... -X DELETE` had no DRY_RUN check (gt-lj13).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0

WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "$WORK_ROOT"' EXIT

# Fake "GitHub" remote: a local bare repo whose path contains github.com/<org>/<repo>
# so run.sh's `sed -E 's|.*github\.com[:/]||; s|\.git$||'` derives a realistic GH_REPO
# slug without touching the network.
BARE_DIR="$WORK_ROOT/github.com/test-org/test-repo.git"
mkdir -p "$(dirname "$BARE_DIR")"
git init --bare -q "$BARE_DIR"

WORK_DIR="$WORK_ROOT/work"
git clone -q "$BARE_DIR" "$WORK_DIR"
git -C "$WORK_DIR" config user.email "test@example.com"
git -C "$WORK_DIR" config user.name "Test"
git -C "$WORK_DIR" commit --allow-empty -q -m "initial"
git -C "$WORK_DIR" branch -M main
git -C "$WORK_DIR" push -q origin main

# A remote branch that matches REMOTE_PATTERNS and is an ancestor of main
# (identical to it), i.e. eligible for deletion.
git -C "$WORK_DIR" push -q origin main:refs/heads/polecat/testbranch
git -C "$WORK_DIR" fetch -q --prune --all

# Stub `gt` (rig enumeration) and `gh` (the destructive remote call) on PATH.
STUB_BIN="$WORK_ROOT/bin"
mkdir -p "$STUB_BIN"
GH_CALLS_LOG="$WORK_ROOT/gh_calls.log"
: > "$GH_CALLS_LOG"

cat > "$STUB_BIN/gt" <<EOF
#!/usr/bin/env bash
if [ "\$1" = "rig" ] && [ "\$2" = "list" ]; then
  echo '[{"repo_path": "$WORK_DIR"}]'
  exit 0
fi
exit 1
EOF
chmod +x "$STUB_BIN/gt"

cat > "$STUB_BIN/gh" <<'EOF'
#!/usr/bin/env bash
echo "$@" >> "$GH_CALLS_LOG"
if [ "$1" = "api" ]; then
  exit 0
fi
exit 1
EOF
chmod +x "$STUB_BIN/gh"

# Stub `bd` so the run's end-of-script receipt (bd create/close) never touches
# the real Dolt database.
cat > "$STUB_BIN/bd" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "$STUB_BIN/bd"

run_hygiene() {
  local extra_arg="${1:-}"
  PATH="$STUB_BIN:$PATH" GH_CALLS_LOG="$GH_CALLS_LOG" bash "$SCRIPT_DIR/run.sh" $extra_arg
}

remote_branch_exists() {
  git -C "$BARE_DIR" show-ref --verify --quiet refs/heads/polecat/testbranch
}

# --- Test 1: flagless (report-only) run must NOT delete the remote branch ---

: > "$GH_CALLS_LOG"
OUTPUT="$(run_hygiene 2>&1)" || { echo "FAIL: flagless run exited non-zero"; FAILURES=$((FAILURES + 1)); }

if ! remote_branch_exists; then
  echo "FAIL: flagless run deleted the remote branch (should be report-only)"
  FAILURES=$((FAILURES + 1))
fi

if grep -q -- "-X DELETE" "$GH_CALLS_LOG"; then
  echo "FAIL: flagless run invoked 'gh api ... -X DELETE' (guard did not gate it)"
  FAILURES=$((FAILURES + 1))
fi

if ! echo "$OUTPUT" | grep -q "WOULD delete remote: origin/polecat/testbranch"; then
  echo "FAIL: flagless run did not report the remote branch it would delete"
  FAILURES=$((FAILURES + 1))
fi

# --- Test 2: --destroy actually deletes the remote branch ---

: > "$GH_CALLS_LOG"
run_hygiene --destroy >/dev/null 2>&1 || { echo "FAIL: --destroy run exited non-zero"; FAILURES=$((FAILURES + 1)); }

if ! grep -q -- "-X DELETE" "$GH_CALLS_LOG"; then
  echo "FAIL: --destroy run never invoked the remote DELETE"
  FAILURES=$((FAILURES + 1))
fi

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES test(s) failed"
  exit 1
else
  echo "PASSED: all tests passed"
fi
