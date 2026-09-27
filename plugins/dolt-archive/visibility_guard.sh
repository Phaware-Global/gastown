#!/usr/bin/env bash
# visibility_guard.sh — refuse to push a Dolt remote or the JSONL git backup
# remote unless it is a GitHub repo confirmed PRIVATE. Sourced by run.sh; also sourced directly by
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
#
# The userinfo class excludes '/', '@', '#', '?', and whitespace: each of
# those terminates the authority section per the URL spec, so a userinfo
# containing one (e.g. "evil#@github.com/priv/repo") would let a real parser
# resolve a different host than this regex does. Anything that could produce
# that host differential must fail to parse here, not silently resolve to
# github.com.
github_owner_repo() {
  local url="$1"
  url="${url#git+}"

  if [[ "$url" =~ ^(https?|ssh)://([^/@#?[:space:]]+@)?github\.com[:/](\./)?([^/[:space:]]+/[^/[:space:]]+)$ ]]; then
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

# git_push_allowed REMOTE
#
# The git-backup counterpart of remote_push_allowed (gt-sg6n): judges where
# `git push REMOTE` would ACTUALLY send data, for the repo in the current
# directory. It asks git for the effective push URL(s) — `git remote get-url
# --push --all` — so a pushurl, url.<base>.insteadOf or pushInsteadOf rewrite
# is judged by its result, not by the raw remote.<name>.url. A remote with
# several push URLs is pushed to all of them, so EVERY one must be confirmed
# private; the first that is not decides the refusal.
#
# Same contract as remote_push_allowed: one-word reason on stdout, return 0
# only when every push URL is a confirmed-private GitHub repo. An unresolvable
# remote returns 1 with "push-url-unresolvable" — fail closed.
git_push_allowed() {
  local remote="$1"
  local raw url reason
  local urls=()

  if ! raw="$(git remote get-url --push --all "$remote" 2>/dev/null)" || [[ -z "$raw" ]]; then
    echo "push-url-unresolvable"
    return 1
  fi

  IFS=$'\n' read -r -d '' -a urls <<< "$raw" || true
  if [[ ${#urls[@]} -eq 0 ]]; then
    echo "push-url-unresolvable"
    return 1
  fi

  for url in "${urls[@]}"; do
    if ! reason="$(remote_push_allowed "$url")"; then
      echo "$reason"
      return 1
    fi
  done

  echo "private"
  return 0
}
