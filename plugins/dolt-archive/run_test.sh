#!/usr/bin/env bash
# run_test.sh — integration test for run.sh's Dolt-push visibility guard.
#
# Runs the REAL run.sh (not a copy) end-to-end against a scratch DOLT_DATA_DIR
# with fake `dolt`/`gh`/`bd`/`gt` binaries shimmed onto PATH. Nothing here
# touches a real remote, real Dolt server, or the production databases — per
# gt-v3df's instruction to use a scratch repo and never attempt a real push
# while testing.
#
# Usage: bash plugins/dolt-archive/run_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_SH="$SCRIPT_DIR/run.sh"

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
  repo="${2#repos/}"
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
echo "run_test.sh: $PASS passed, $FAIL failed"
if [[ "$FAIL" -ne 0 ]]; then
  echo "--- run.sh output ---"
  cat "$OUTPUT"
fi
[[ "$FAIL" -eq 0 ]]
