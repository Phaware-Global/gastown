#!/usr/bin/env bash
# gocache-trim/run.sh — keep the shared Go build cache under a size cap.
#
# Go only drops entries unused for 5 days, so many worktrees building with
# different flags can fill a disk well before that. Over the cap, this removes
# the entries unused longest, the same way Go's own trim does.
#
# Safe while builds are running: go refreshes an entry's mtime (at most hourly)
# whenever a build uses it, and treats a missing entry as a cache miss. The
# youngest age removed leaves an hour of headroom over that refresh interval,
# so only a single build running for over an hour could lose an input.

set -euo pipefail

CAP_MB="${GT_GOCACHE_CAP_MB:-10240}"
# Minutes since last use, tried in order until the cache fits. Never below 120.
AGES="1440 720 360 120"

log() { echo "[gocache-trim] $*"; }

record_run() {
  local rid
  rid="$(bd create "gocache-trim: $1" -t chore --ephemeral \
    -l type:plugin-run,plugin:gocache-trim,result:success \
    --silent 2>/dev/null)" || true
  [ -n "${rid:-}" ] && bd close "$rid" --reason "plugin run recorded" >/dev/null 2>&1 || true
}

cache_dir() {
  if command -v go >/dev/null 2>&1; then
    go env GOCACHE
  elif [ -n "${GOCACHE:-}" ]; then
    echo "$GOCACHE"
  elif [ "$(uname)" = "Darwin" ]; then
    echo "$HOME/Library/Caches/go-build"
  else
    echo "${XDG_CACHE_HOME:-$HOME/.cache}/go-build"
  fi
}

size_mb() {
  local out
  # du exits non-zero when a build removes a file mid-walk; its total still counts.
  out="$(du -sk "$CACHE/" 2>/dev/null || true)"
  [ -n "$out" ] || return 1
  echo $(( ${out%%[!0-9]*} / 1024 ))
}

if ! [[ "$CAP_MB" =~ ^[1-9][0-9]*$ ]]; then
  log "GT_GOCACHE_CAP_MB must be a positive integer, got '$CAP_MB'"
  exit 1
fi

CACHE="$(cache_dir)"
if [ "$CACHE" = "off" ] || [ ! -d "$CACHE" ]; then
  log "No Go build cache at '$CACHE'. Nothing to do."
  exit 0
fi

SIZE="$(size_mb)" || { log "Cannot measure $CACHE"; exit 1; }
log "Cache $CACHE: ${SIZE}MB (cap: ${CAP_MB}MB)"
if [ "$SIZE" -le "$CAP_MB" ]; then
  record_run "${SIZE}MB, under ${CAP_MB}MB cap"
  exit 0
fi

# Delete only from a directory Go says is its cache.
if ! grep -q "Go build system" "$CACHE/README" 2>/dev/null; then
  log "Refusing to trim $CACHE: no Go build cache README"
  exit 1
fi

BEFORE="$SIZE"
for age in $AGES; do
  log "Removing entries unused for ${age} minutes"
  find "$CACHE"/[0-9a-f][0-9a-f] -mindepth 1 -maxdepth 1 \
    \( -name '*-a' -o -name '*-d' \) -mmin "+$age" -exec rm -rf {} + ||
    log "Some entries could not be removed"
  SIZE="$(size_mb)" || { log "Cannot measure $CACHE"; exit 1; }
  [ "$SIZE" -le "$CAP_MB" ] && break
done

SUMMARY="trimmed ${BEFORE}MB -> ${SIZE}MB (cap: ${CAP_MB}MB)"
if [ "$SIZE" -gt "$CAP_MB" ]; then
  SUMMARY="$SUMMARY, still over cap: the rest was used too recently to remove"
fi
log "$SUMMARY"
record_run "$SUMMARY"
