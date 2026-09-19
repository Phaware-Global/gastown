#!/usr/bin/env bash
# visibility_guard.sh — refuse to push a Dolt remote unless it is a GitHub
# repo confirmed PRIVATE. Sourced by run.sh; also sourced directly by
# run_test.sh for unit tests, so it must have no side effects of its own
# (no top-level execution, nothing that touches the network or Dolt).
#
# Fail-closed by design: a public repo, a non-GitHub remote, an unparseable
# URL, a missing `gh`, or a failed visibility lookup are all treated the
# same as "public" — push does not happen.

GH_VISIBILITY_TIMEOUT="${GH_VISIBILITY_TIMEOUT:-15}"

# Extract "owner/repo" from a github.com remote URL on stdout; fails (exit 1,
# nothing printed) for anything else. Handles the URL forms Dolt's git-backed
# remotes use (git+https://, git+ssh://, including the /./ segment Dolt
# inserts when it rewrites an scp-style remote to git+ssh://git@github.com/./owner/repo),
# plain https/ssh git URLs, the git@github.com: scp-style form, and an
# embedded userinfo/token (https://x-access-token:TOKEN@github.com/...).
# DoltHub remotes (doltremoteapi.dolthub.com) and any other host fall through
# to the final `return 1` — deliberately: this function only vouches for
# GitHub remotes, and the caller must refuse anything it doesn't vouch for.
github_owner_repo() {
  local url="$1"
  url="${url#git+}"

  if [[ "$url" =~ ^(https?|ssh)://([^/@[:space:]]+@)?github\.com[:/](\./)?([^/[:space:]]+/[^/[:space:]]+)$ ]]; then
    url="${BASH_REMATCH[4]}"
  elif [[ "$url" =~ ^git@github\.com:([^/[:space:]]+/[^/[:space:]]+)$ ]]; then
    url="${BASH_REMATCH[1]}"
  else
    return 1
  fi

  url="${url%.git}"
  url="${url%/}"
  [[ "$url" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || return 1
  printf '%s\n' "$url"
}

# remote_push_allowed URL
#
# Prints a one-word reason on stdout and returns 0 only when the remote is a
# GitHub repo whose visibility API confirms "private". Any other outcome
# returns 1 with the refusal reason on stdout ("public", "internal",
# "non-github-remote", "gh-unavailable", or "visibility-lookup-failed") —
# the caller must not push when this returns non-zero.
remote_push_allowed() {
  local url="$1"
  local owner_repo visibility

  if ! command -v gh >/dev/null 2>&1; then
    echo "gh-unavailable"
    return 1
  fi

  if ! owner_repo="$(github_owner_repo "$url")"; then
    echo "non-github-remote"
    return 1
  fi

  if ! visibility="$(timeout "$GH_VISIBILITY_TIMEOUT" gh api --hostname github.com "repos/$owner_repo" --jq '.visibility' 2>/dev/null)"; then
    echo "visibility-lookup-failed"
    return 1
  fi
  visibility="$(printf '%s' "$visibility" | tr -d '[:space:]')"

  if [[ "$visibility" == "private" ]]; then
    echo "private"
    return 0
  fi

  if [[ -z "$visibility" ]]; then
    echo "visibility-lookup-failed"
    return 1
  fi

  echo "$visibility"
  return 1
}
