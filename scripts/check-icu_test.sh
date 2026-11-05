#!/usr/bin/env bash
# Tests for the Makefile's macOS ICU4C detection (ICU_PREFIX / check-icu).
#
# Each case runs make with a scrubbed environment and a throwaway `brew` shim,
# so the result does not depend on what is installed on the host. The ICU block
# only exists on Darwin, so the suite is a no-op elsewhere.
set -euo pipefail

if [[ "$(uname)" != "Darwin" ]]; then
  echo "SKIP: ICU detection is Darwin-only"
  exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MAKE_BIN="${MAKE:-/usr/bin/make}"
GO_BIN="$(PATH="${PATH}" command -v go)"
GO_REAL="$(cd "$REPO_ROOT" && "$GO_BIN" env GOROOT)/bin/go"
SYSTEM_PATH="/usr/bin:/bin"
TMPDIR_ROOT=""
PASS=0
FAIL=0

cleanup() {
  if [[ -n "$TMPDIR_ROOT" && -d "$TMPDIR_ROOT" ]]; then
    rm -rf "$TMPDIR_ROOT"
  fi
}
trap cleanup EXIT

TMPDIR_ROOT="$(mktemp -d)"
FAKE_HOME="$TMPDIR_ROOT/home"
GOENV_FILE="$TMPDIR_ROOT/goenv"
BREW_LOG="$TMPDIR_ROOT/brew.log"
GO_DIR="$TMPDIR_ROOT/godir"
BREW_DIR="$TMPDIR_ROOT/brewdir"
mkdir -p "$FAKE_HOME" "$GO_DIR" "$BREW_DIR"
ln -s "$GO_REAL" "$GO_DIR/go"

# make_keg NAME -> prints a fake keg prefix containing unicode/regex.h.
make_keg() {
  local keg="$TMPDIR_ROOT/Cellar/$1"
  mkdir -p "$keg/include/unicode" "$keg/lib"
  : > "$keg/include/unicode/regex.h"
  echo "$keg"
}

# write_brew ALIAS_PREFIX INSTALLED_PREFIX FORMULAE VERSIONED_NAME VERSIONED_PREFIX
# Writes a brew shim modelling the subcommands the Makefile uses:
#   brew --prefix icu4c            -> ALIAS_PREFIX   (exit 0, even if nonexistent)
#   brew --prefix --installed icu4c-> INSTALLED_PREFIX (empty + exit 1 if unset)
#   brew list --formula            -> FORMULAE
#   brew --prefix <versioned>      -> VERSIONED_PREFIX
write_brew() {
  local alias_prefix="$1" installed_prefix="$2" formulae="$3" vname="$4" vprefix="$5"
  cat > "$BREW_DIR/brew" <<EOF
#!/bin/sh
echo "brew \$*" >> "$BREW_LOG"
case "\$*" in
  "--prefix --installed icu4c")
    if [ -n "$installed_prefix" ]; then echo "$installed_prefix"; exit 0; fi
    exit 1 ;;
  "--prefix icu4c") echo "$alias_prefix"; exit 0 ;;
  "list --formula") printf '%s\n' $formulae; exit 0 ;;
  "--prefix $vname") echo "$vprefix"; exit 0 ;;
esac
exit 1
EOF
  chmod +x "$BREW_DIR/brew"
}

remove_brew() { rm -f "$BREW_DIR/brew"; }

# A probe target so tests can see the CGO flags make exports to recipes.
cat > "$TMPDIR_ROOT/probe.mk" <<'EOF'
probe: check-icu
	@echo "CPP=$$CGO_CPPFLAGS"
	@echo "LD=$$CGO_LDFLAGS"
EOF

# run_make PATH_VALUE [VAR=value ...] -> output on stdout, make's status in $RC.
run_make() {
  local path_value="$1"; shift
  RC=0
  : > "$BREW_LOG"
  OUT="$(env -i PATH="$path_value" HOME="$FAKE_HOME" GOENV="$GOENV_FILE" \
      GOTOOLCHAIN=local GOFLAGS=-mod=mod "$@" \
      "$MAKE_BIN" --no-print-directory -C "$REPO_ROOT" \
      -f Makefile -f "$TMPDIR_ROOT/probe.mk" probe 2>&1)" || RC=$?
}

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "$OUT" | sed 's/^/        /'; FAIL=$((FAIL + 1)); }

assert_ok_with() {
  local name="$1" want_cpp="$2" want_ld="$3"
  if [[ "$RC" -eq 0 && "$OUT" == *"CPP=$want_cpp"* && "$OUT" == *"LD=$want_ld"* ]]; then
    pass "$name"
  else
    fail "$name (rc=$RC)"
  fi
}

assert_icu_error() {
  local name="$1"
  if [[ "$RC" -eq 2 && "$OUT" == *"ICU4C headers (unicode/regex.h) not found"* ]]; then
    pass "$name"
  else
    fail "$name (rc=$RC)"
  fi
}

reset_goenv() { : > "$GOENV_FILE"; }

echo "Running ICU detection tests..."

KEG78="$(make_keg icu4c@78)"
MISSING79="$TMPDIR_ROOT/Cellar/icu4c@79"   # never created

# 1. The unversioned alias moved to a keg that is not installed.
reset_goenv
write_brew "$MISSING79" "$MISSING79" "icu4c@78 wget" "icu4c@78" "$KEG78"
run_make "$BREW_DIR:$GO_DIR:$SYSTEM_PATH"
assert_ok_with "alias points at an uninstalled keg: falls back to the installed one" \
  "-I$KEG78/include" "-L$KEG78/lib"

# 2. Only a versioned keg is installed (--installed prints nothing).
reset_goenv
write_brew "$MISSING79" "" "icu4c@78 wget" "icu4c@78" "$KEG78"
run_make "$BREW_DIR:$GO_DIR:$SYSTEM_PATH"
assert_ok_with "only a versioned keg installed: found via brew list" \
  "-I$KEG78/include" "-L$KEG78/lib"

# 2b. Several versioned kegs: the newest wins, numerically (78 < 100).
KEG100="$(make_keg icu4c@100)"
reset_goenv
write_brew "$MISSING79" "" "icu4c@78 icu4c@100 icu4c@9" "icu4c@100" "$KEG100"
run_make "$BREW_DIR:$GO_DIR:$SYSTEM_PATH"
assert_ok_with "several versioned kegs: newest major wins numerically" \
  "-I$KEG100/include" "-L$KEG100/lib"

# 3. An ICU_PREFIX in the environment that contains include/unicode/regex.h
#    wins and the brew lookup is skipped.
reset_goenv
OVERRIDE="$(make_keg override)"
write_brew "$MISSING79" "$MISSING79" "icu4c@78" "icu4c@78" "$KEG78"
run_make "$BREW_DIR:$GO_DIR:$SYSTEM_PATH" ICU_PREFIX="$OVERRIDE"
assert_ok_with "ICU_PREFIX override is used" \
  "-I$OVERRIDE/include" "-L$OVERRIDE/lib"
if [[ ! -s "$BREW_LOG" ]]; then
  pass "ICU_PREFIX override does not invoke brew"
else
  OUT="$(cat "$BREW_LOG")"; fail "ICU_PREFIX override invoked brew"
fi

# 3b. An ICU_PREFIX that lacks include/unicode/regex.h is discarded in favour
#     of the newest installed brew keg.
reset_goenv
write_brew "$MISSING79" "$KEG78" "icu4c@78" "icu4c@78" "$KEG78"
run_make "$BREW_DIR:$GO_DIR:$SYSTEM_PATH" ICU_PREFIX="$TMPDIR_ROOT/nope"
# The build must never proceed with the broken prefix.
if [[ "$RC" -eq 0 && "$OUT" != *"$TMPDIR_ROOT/nope"* ]]; then
  pass "bad ICU_PREFIX never reaches the CGO flags"
else
  fail "bad ICU_PREFIX leaked into the CGO flags (rc=$RC)"
fi

# 4. CGO flags supplied by the caller, no brew at all.
reset_goenv
remove_brew
run_make "$GO_DIR:$SYSTEM_PATH" CGO_CPPFLAGS="-I/opt/local/include"
assert_ok_with "CGO_CPPFLAGS supplied, no brew: accepted" "-I/opt/local/include" ""

reset_goenv
run_make "$GO_DIR:$SYSTEM_PATH" CGO_CXXFLAGS="-I/opt/local/include"
if [[ "$RC" -eq 0 ]]; then
  pass "CGO_CXXFLAGS supplied, no brew: accepted"
else
  fail "CGO_CXXFLAGS supplied, no brew: rejected (rc=$RC)"
fi

# 5. Nothing anywhere: fail with the actionable message.
reset_goenv
remove_brew
run_make "$GO_DIR:$SYSTEM_PATH"
assert_icu_error "no brew, no flags: check-icu fails with a hint"

# 6. ICU flags persisted with `go env -w` (the GOENV file) are honoured: go
# itself builds go-icu-regex with them, so the Makefile must not reject them.
reset_goenv
remove_brew
env -i PATH="$GO_DIR:$SYSTEM_PATH" HOME="$FAKE_HOME" GOENV="$GOENV_FILE" GOTOOLCHAIN=local \
  go env -w "CGO_CPPFLAGS=-I$KEG78/include" "CGO_LDFLAGS=-L$KEG78/lib"
run_make "$GO_DIR:$SYSTEM_PATH"
if [[ "$RC" -eq 0 ]]; then
  pass "ICU flags in the GOENV file, no brew: accepted"
else
  fail "ICU flags in the GOENV file, no brew: rejected (rc=$RC)"
fi

# 7. go's default CGO_CXXFLAGS (-O2 -g) must not count as caller-supplied ICU.
reset_goenv
remove_brew
run_make "$GO_DIR:$SYSTEM_PATH"
assert_icu_error "go's default CGO_CXXFLAGS does not mask a missing ICU"

echo ""
echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
