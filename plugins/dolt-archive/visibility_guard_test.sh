#!/usr/bin/env bash
# visibility_guard_test.sh — unit tests for visibility_guard.sh.
#
# No real dolt/gh/network calls: `gh` is a fake binary shimmed onto PATH for
# the duration of the run, and no remote config or push is ever touched.
#
# Usage: bash plugins/dolt-archive/visibility_guard_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/visibility_guard.sh"

FAKE_BIN="$(mktemp -d)"
EMPTY_BIN="$(mktemp -d)"
trap 'rm -rf "$FAKE_BIN" "$EMPTY_BIN"' EXIT

cat > "$FAKE_BIN/gh" <<'FAKE_GH'
#!/usr/bin/env bash
# Fake `gh` for tests: `gh api repos/OWNER/REPO --jq '.visibility'`.
# Visibility is driven by the owner segment of the repo path so each test
# case can pick a scenario just by choosing a remote URL.
if [[ "$1" == "api" ]]; then
  repo="${2#repos/}"
  owner="${repo%%/*}"
  case "$owner" in
    pub-owner)    echo "public" ;;
    priv-owner)   echo "private" ;;
    internal-owner) echo "internal" ;;
    lookup-fails-owner) exit 1 ;;
    empty-owner) echo "" ;;
    *) echo "public" ;;
  esac
  exit 0
fi
exit 1
FAKE_GH
chmod +x "$FAKE_BIN/gh"

PASS=0
FAIL=0

# assert_parse URL EXPECTED_OWNER_REPO_OR_EMPTY
assert_parse() {
  local url="$1" expect="$2" got
  if got="$(github_owner_repo "$url")"; then
    :
  else
    got=""
  fi
  if [[ "$got" == "$expect" ]]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "FAIL github_owner_repo($url) = '$got', want '$expect'"
  fi
}

# assert_guard URL EXPECTED_ALLOWED(0|1) EXPECTED_REASON [PATH_OVERRIDE]
assert_guard() {
  local url="$1" expect_allowed="$2" expect_reason="$3"
  local path_override="${4:-$FAKE_BIN:$PATH}"
  local reason got_allowed
  if reason="$(PATH="$path_override" remote_push_allowed "$url")"; then
    got_allowed=0
  else
    got_allowed=1
  fi
  if [[ "$got_allowed" == "$expect_allowed" && "$reason" == "$expect_reason" ]]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "FAIL remote_push_allowed($url) = (allowed=$got_allowed reason=$reason), want (allowed=$expect_allowed reason=$expect_reason)"
  fi
}

# --- URL parsing --------------------------------------------------------

assert_parse "git+https://github.com/priv-owner/repo" "priv-owner/repo"
assert_parse "https://github.com/priv-owner/repo.git" "priv-owner/repo"
assert_parse "git@github.com:priv-owner/repo.git" "priv-owner/repo"
assert_parse "ssh://git@github.com/priv-owner/repo.git" "priv-owner/repo"
assert_parse "https://x-access-token:TOKEN@github.com/priv-owner/repo.git" "priv-owner/repo"
assert_parse "https://doltremoteapi.dolthub.com/priv-owner/repo" ""
assert_parse "not-a-url" ""
assert_parse "git+https://github.com/only-owner" ""

# --- Required scenarios from gt-v3df ------------------------------------

# 1. A remote pointing at a public repo -> refused.
assert_guard "git+https://github.com/pub-owner/repo" 1 "public"

# 2. A remote pointing at a private repo -> push allowed.
assert_guard "git+https://github.com/priv-owner/repo" 0 "private"

# 3. Visibility lookup fails -> refused (fail closed).
assert_guard "git+https://github.com/lookup-fails-owner/repo" 1 "visibility-lookup-failed"

# --- Additional fail-closed cases required by gt-v3df's rule #2 ---------

# A non-GitHub remote (e.g. DoltHub) -> refused, never silently allowed.
assert_guard "https://doltremoteapi.dolthub.com/priv-owner/repo" 1 "non-github-remote"

# An unparseable URL -> refused.
assert_guard "garbage" 1 "non-github-remote"

# A visibility other than "public"/"private" (e.g. GH Enterprise "internal")
# is not "confirmed private" -> refused, not silently allowed.
assert_guard "git+https://github.com/internal-owner/repo" 1 "internal"

# An empty/unparseable visibility response -> refused.
assert_guard "git+https://github.com/empty-owner/repo" 1 "visibility-lookup-failed"

# gh missing entirely -> refused, never silently allowed.
assert_guard "git+https://github.com/priv-owner/repo" 1 "gh-unavailable" "$EMPTY_BIN"

echo ""
echo "visibility_guard_test.sh: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
