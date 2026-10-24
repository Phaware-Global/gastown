// Package remoteguard decides whether data may be pushed to a Dolt remote.
// The Dolt databases hold the town's issues and mail, so a push is allowed
// only to a GitHub repo whose visibility API reports "private"; every other
// outcome refuses. The daemon's dolt_remotes patrol and `gt dolt sync` share
// this one implementation, and plugins/dolt-archive/visibility_guard.sh is
// its shell counterpart.
package remoteguard

import (
	"context"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

const (
	remoteVisibilityTimeout = 15 * time.Second

	// doltHubRemoteHost is the fixed host SetupDoltHubRemote (gt dolt sync)
	// adds as "origin" — see internal/doltserver/dolthub.go's
	// dolthubRemoteBase / DoltHubRemoteURL. It is a literal hostname, never
	// a target for the userinfo/port tricks githubOwnerRepo below guards
	// against.
	doltHubRemoteHost = "doltremoteapi.dolthub.com"
)

// githubSCPRe matches the scp-style remote form (git@github.com:owner/repo).
// This is not a URL, so it is parsed with its own exact, anchored pattern
// rather than through net/url — Dolt itself never produces this form (it
// rewrites scp-style remotes into a git+ssh URL, handled in githubOwnerRepo
// below), but a hand-added remote still might use it.
//
// ownerRepoRe then validates whatever either path extracts against GitHub's
// actual owner/repo character set. This isn't cosmetic: without it, the scp
// regex above would happily extract "x@evil.com/repo" as an "owner/repo"
// pair, reintroducing inside this narrow path the exact userinfo-in-authority
// confusion that switching to net/url.Hostname() exists to eliminate.
var (
	githubSCPRe = regexp.MustCompile(`^git@github\.com:([^/\s]+/[^/\s]+)$`)
	ownerRepoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// githubOwnerRepo extracts "owner/repo" from a github.com remote URL. It
// returns ("", false) for anything it doesn't confidently recognize as a
// github.com remote — the caller must not push when this returns false.
//
// This used to be hand-rolled regex (plugins/dolt-archive/visibility_guard.sh's
// github_owner_repo, ported here). Across three review rounds on PR #238 and
// a fourth on PR #239, an adversarial reviewer kept finding a new parser gap
// in it — most recently https://github.com:x@evil.com/repo, where the ':'
// after "github.com" is userinfo syntax (user:password@host), not a port,
// so the URL actually vouches for evil.com. That is exactly the class of bug
// net/url's Hostname() exists to not have: it strips userinfo and port for
// you, the same way it does for every other caller in the standard library,
// instead of re-deriving the answer with a regex that can be wrong again.
func githubOwnerRepo(rawURL string) (string, bool) {
	raw := strings.TrimPrefix(rawURL, "git+")

	if m := githubSCPRe.FindStringSubmatch(raw); m != nil {
		return normalizeOwnerRepo(m[1])
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch u.Scheme {
	case "http", "https", "ssh":
	default:
		return "", false
	}
	if u.Hostname() != "github.com" {
		return "", false
	}

	path := strings.TrimPrefix(u.Path, "/")
	// Dolt rewrites an scp-style remote into git+ssh://git@github.com/./owner/repo (gt-lc57).
	path = strings.TrimPrefix(path, "./")
	return normalizeOwnerRepo(path)
}

// normalizeOwnerRepo strips a trailing ".git" and validates what's left
// against GitHub's owner/repo character set.
func normalizeOwnerRepo(ownerRepo string) (string, bool) {
	ownerRepo = strings.TrimSuffix(ownerRepo, ".git")
	ownerRepo = strings.TrimSuffix(ownerRepo, "/")
	if !ownerRepoRe.MatchString(ownerRepo) {
		return "", false
	}
	return ownerRepo, true
}

// isDoltHubRemote reports whether rawURL is a DoltHub push/pull remote —
// i.e. it resolves, via the same net/url parsing githubOwnerRepo uses, to
// the fixed host SetupDoltHubRemote adds as "origin". Checked explicitly
// and separately from the generic "not GitHub" refusal so its reason can
// name DoltHub instead of reading like an unrecognized or broken remote.
func isDoltHubRemote(rawURL string) bool {
	raw := strings.TrimPrefix(rawURL, "git+")
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Hostname() == doltHubRemoteHost
}

// ghLookPath resolves the `gh` binary; overridden in tests to simulate it
// being unavailable without touching the real PATH.
var ghLookPath = func() error {
	_, err := exec.LookPath("gh")
	return err
}

// ghVisibilityLookup queries GitHub for a repo's visibility, pinned to
// github.com so a machine configured for a different default host (e.g.
// GHES) can't answer for a github.com remote. Overridden in tests.
var ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteVisibilityTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", "repos/"+ownerRepo, "--jq", ".visibility")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PushAllowed reports whether a push to the given Dolt remote URL is
// allowed: only when it resolves to a GitHub repo whose visibility API
// confirms "private". It fails closed on everything else — a public repo,
// a non-GitHub remote, an unparseable URL, a missing `gh`, or a failed or
// empty visibility lookup are all refused — mirroring
// visibility_guard.sh's remote_push_allowed. The returned reason is one of
// "private" (allowed), or "public"/other-visibility, "non-github-remote",
// "dolthub-unsupported", "gh-unavailable", "visibility-lookup-failed"
// (refused).
//
// DoltHub remotes — including SetupDoltHubRemote's own "origin", which it
// creates private — are refused explicitly rather than falling through to
// "non-github-remote". DoltHub's query-execution API
// (www.dolthub.com/api/v1alpha1) has no visibility field, and its GraphQL
// API (www.dolthub.com/graphql) disables introspection and is otherwise
// undocumented: there is no supported way for this guard to confirm a
// DoltHub repo's visibility today (verified against the live API while
// fixing this, gt-175j). Naming the reason distinctly tells the operator
// this is a known, intentional gap — not a broken guard silently eating
// their backups — same failure shape as the SCP-form bug from #238.
func PushAllowed(ctx context.Context, remoteURL string) (bool, string) {
	if isDoltHubRemote(remoteURL) {
		return false, "dolthub-unsupported"
	}

	if err := ghLookPath(); err != nil {
		return false, "gh-unavailable"
	}

	ownerRepo, ok := githubOwnerRepo(remoteURL)
	if !ok {
		return false, "non-github-remote"
	}

	visibility, err := ghVisibilityLookup(ctx, ownerRepo)
	if err != nil || visibility == "" {
		return false, "visibility-lookup-failed"
	}

	if visibility == "private" {
		return true, "private"
	}
	return false, visibility
}
