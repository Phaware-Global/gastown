#!/usr/bin/env bash
# Tests for Makefile signing of the remote-execution binaries.
#
# On a worker, gt-worker-client is the long-lived process macOS TCC attributes
# the agent's file access to, so it needs the same stable signature gt gets:
# every binary make installs must be signed with GT_CODESIGN_IDENTITY under its
# own identifier, staged-signed-moved, and a failed signature must fail the
# install without replacing what is there.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MAKE_BIN="$(command -v "${MAKE:-make}")"
TMPDIR=""
PASS=0
FAIL=0
BINARIES="gt-worker-attach gt-worker-client gt-proxy-client gt-proxy-server"

cleanup() {
  if [[ -n "$TMPDIR" && -d "$TMPDIR" ]]; then
    rm -rf "$TMPDIR"
  fi
}
trap cleanup EXIT

# setup <codesign exit code>: stub build outputs, an existing install, and a
# fake codesign that records its arguments.
setup() {
  cleanup
  TMPDIR="$(mktemp -d)"
  BUILD="$TMPDIR/build"; INSTALL="$TMPDIR/bin"; ARTIFACTS="$TMPDIR/artifacts"; FAKE="$TMPDIR/fake"
  mkdir -p "$BUILD" "$INSTALL" "$FAKE"
  for b in $BINARIES; do
    echo "new $b" > "$BUILD/$b"
    echo "old $b" > "$INSTALL/$b"
  done
  cat > "$FAKE/codesign" <<FAKE_EOF
#!/bin/sh
echo "\$*" >> "$TMPDIR/codesign.log"
exit $1
FAKE_EOF
  chmod +x "$FAKE/codesign"
}

run_install() {
  env PATH="$FAKE:$PATH" "$MAKE_BIN" --no-print-directory -C "$REPO_ROOT" \
    install-remote-execution-binaries \
    BUILD_DIR="$BUILD" INSTALL_DIR="$INSTALL" ARTIFACT_DIR="$ARTIFACTS" "$@" 2>&1
}

check() {
  local name="$1"; shift
  if "$@"; then
    echo "  PASS: $name"; PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"; FAIL=$((FAIL + 1))
  fi
}

echo "signing: every binary is signed under its own identifier"
setup 0
out="$(run_install GT_CODESIGN_IDENTITY=test-identity)" || { echo "$out"; }
for b in $BINARIES; do
  check "$b signed as com.gastown.$b" \
    grep -q -- "-f -s test-identity --identifier com.gastown.$b $INSTALL/$b.new" "$TMPDIR/codesign.log"
  check "$b installed" grep -q "new $b" "$INSTALL/$b"
  check "$b leaves no staged file" test ! -e "$INSTALL/$b.new"
done

echo "signing: this platform's pushable artifacts are the signed installs"
platform="$(go env GOOS)-$(go env GOARCH)"
for b in gt-proxy-client gt-worker-client; do
  check "$b artifact matches the installed binary" cmp -s "$INSTALL/$b" "$ARTIFACTS/$platform/$b"
done

echo "signing: a failed signature fails the install and replaces nothing"
setup 1
if out="$(run_install GT_CODESIGN_IDENTITY=test-identity)"; then
  echo "  FAIL: make succeeded although codesign failed"; FAIL=$((FAIL + 1))
else
  echo "  PASS: make failed"; PASS=$((PASS + 1))
fi
for b in $BINARIES; do
  check "$b left untouched" grep -q "old $b" "$INSTALL/$b"
  check "$b leaves no staged file" test ! -e "$INSTALL/$b.new"
done

echo "signing: no identity, no codesign"
setup 0
out="$(run_install)" || { echo "$out"; }
check "codesign not invoked" test ! -e "$TMPDIR/codesign.log"
for b in $BINARIES; do
  check "$b installed" grep -q "new $b" "$INSTALL/$b"
done

echo
echo "check-codesign: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
