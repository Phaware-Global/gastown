package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultDoltRemotesInterval = 15 * time.Minute
	doltPushTimeout            = 60 * time.Second
	remoteVisibilityTimeout    = 15 * time.Second

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

// remotePushAllowed reports whether a push to the given Dolt remote URL is
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
func remotePushAllowed(ctx context.Context, remoteURL string) (bool, string) {
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

// doltRemotesInterval returns the configured push interval, or the default (15m).
func doltRemotesInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.DoltRemotes != nil {
		if config.Patrols.DoltRemotes.Interval > 0 {
			return config.Patrols.DoltRemotes.Interval
		}
	}
	return defaultDoltRemotesInterval
}

// pushDoltRemotes commits and pushes each configured database to its remote.
// Non-fatal: errors are logged but don't stop the patrol.
func (d *Daemon) pushDoltRemotes() {
	if !d.isPatrolActive("dolt_remotes") {
		return
	}

	// Need dolt server to be configured for data dir
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		d.logger.Printf("dolt_remotes: dolt server not configured, skipping")
		return
	}

	dataDir := d.doltServer.config.DataDir
	if dataDir == "" {
		d.logger.Printf("dolt_remotes: no data dir configured, skipping")
		return
	}

	config := d.patrolConfig.Patrols.DoltRemotes
	remote := config.Remote
	branch := config.Branch
	if branch == "" {
		branch = "main"
	}

	// Get list of databases to push.
	// When a specific remote is configured, filter by it.
	// When no remote is configured, discover databases with any remote.
	databases := config.Databases
	if len(databases) == 0 {
		var err error
		if remote != "" {
			databases, err = d.discoverDatabasesWithRemotes(dataDir, remote)
		} else {
			databases, err = d.discoverDatabasesWithAnyRemote(dataDir)
		}
		if err != nil {
			d.logger.Printf("dolt_remotes: error discovering databases: %v", err)
			return
		}
	}

	if len(databases) == 0 {
		d.logger.Printf("dolt_remotes: no databases with remotes found")
		return
	}

	if remote != "" {
		d.logger.Printf("dolt_remotes: pushing %d database(s) to %s/%s", len(databases), remote, branch)
	} else {
		d.logger.Printf("dolt_remotes: pushing %d database(s) (auto-detected remotes)/%s", len(databases), branch)
	}

	pushed := 0
	for _, db := range databases {
		pushRemote := remote
		if pushRemote == "" {
			// Auto-detect the remote name for this database
			pushRemote = d.findDatabaseRemote(dataDir, db)
			if pushRemote == "" {
				d.logger.Printf("dolt_remotes: %s: no remote found, skipping", db)
				continue
			}
		}
		if err := d.pushDatabase(dataDir, db, pushRemote, branch); err != nil {
			d.logger.Printf("dolt_remotes: %s: push failed: %v", db, err)
		} else {
			pushed++
		}
	}

	d.logger.Printf("dolt_remotes: pushed %d/%d database(s)", pushed, len(databases))
}

// pushDatabase commits pending changes and pushes a single database to its remote.
func (d *Daemon) pushDatabase(dataDir, db, remote, branch string) error {
	// Safety: refuse to push anything that looks like a test database.
	// This is the last line of defense against pushing pollution to GitHub.
	for _, prefix := range []string{"test", "beads_t", "beads_pt", "doctest_"} {
		if strings.HasPrefix(db, prefix) {
			return fmt.Errorf("REFUSED: %q looks like a test database (prefix %q)", db, prefix)
		}
	}

	// Safety: refuse to push anywhere except a confirmed-private GitHub
	// remote. This is the destination check the test-name guard above
	// cannot provide: a production database with a perfectly normal name
	// pushed to a public remote passes that guard cleanly.
	remoteURL := d.resolveRemoteURL(dataDir, db, remote)
	if remoteURL == "" {
		return fmt.Errorf("REFUSED: %q: could not resolve URL for remote %q", db, remote)
	}
	if allowed, reason := remotePushAllowed(context.Background(), remoteURL); !allowed {
		return fmt.Errorf("REFUSED: %q: remote %q is not a confirmed-private GitHub repo (%s)", db, remote, reason)
	}

	// Step 1: Stage any unstaged changes (non-fatal)
	addQuery := fmt.Sprintf("USE `%s`; CALL DOLT_ADD('-A')", db)
	if err := d.runDoltSQL(dataDir, addQuery); err != nil {
		// Ignore - may have nothing to stage
		d.logger.Printf("dolt_remotes: %s: add (non-fatal): %v", db, err)
	}

	// Step 2: Commit staged changes only if dolt_status shows pending work.
	// Skipping DOLT_COMMIT when nothing is staged avoids "nothing to commit"
	// warnings in dolt.log, which were causing log bloat at ~3/sec (gt-zb8).
	if d.hasStagedChanges(dataDir, db) {
		commitQuery := fmt.Sprintf(
			"USE `%s`; CALL DOLT_COMMIT('-m', 'daemon: auto-commit pending changes', '--author', 'Gas Town Daemon <daemon@gastown.local>')",
			db,
		)
		if err := d.runDoltSQL(dataDir, commitQuery); err != nil {
			d.logger.Printf("dolt_remotes: %s: commit (non-fatal): %v", db, err)
		}
	}

	// Step 3: Push to remote
	pushQuery := fmt.Sprintf("USE `%s`; CALL DOLT_PUSH('%s', '%s')", db, remote, branch)
	if err := d.runDoltSQL(dataDir, pushQuery); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	d.logger.Printf("dolt_remotes: %s: pushed to %s/%s", db, remote, branch)
	return nil
}

// runDoltSQL executes a SQL query against the Dolt data directory.
func (d *Daemon) runDoltSQL(dataDir, query string) error {
	ctx, cancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "dolt", "sql", "-q", query)
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
	}

	return nil
}

// hasStagedChanges returns true if the database has staged changes in dolt_status.
// Uses dolt_status WHERE staged=1. Fails open (returns true) on query errors so
// that a DOLT_COMMIT attempt is still made and the error is surfaced normally.
func (d *Daemon) hasStagedChanges(dataDir, db string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer cancel()

	query := fmt.Sprintf("USE `%s`; SELECT COUNT(*) FROM dolt_status WHERE staged = 1", db)
	cmd := exec.CommandContext(ctx, "dolt", "sql", "-r", "csv", "-q", query)
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		// Fail open: if we can't check, attempt the commit and let it fail naturally.
		return true
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return false
	}
	return strings.TrimSpace(lines[1]) != "0"
}

// discoverDatabasesWithRemotes lists databases in the data directory
// that have the specified remote configured.
func (d *Daemon) discoverDatabasesWithRemotes(dataDir, remote string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("reading data dir: %w", err)
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip hidden directories
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Check if this directory is a Dolt database (has .dolt subdirectory)
		doltDir := filepath.Join(dataDir, name, ".dolt")
		if _, err := os.Stat(doltDir); os.IsNotExist(err) {
			continue
		}
		// Check if it has the specified remote
		if d.databaseHasRemote(dataDir, name, remote) {
			databases = append(databases, name)
		}
	}

	return databases, nil
}

// escapeSQL escapes single quotes and backslashes for safe SQL string interpolation.
func escapeSQL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "'", "''")
}

// resolveRemoteURL returns the URL configured for a named Dolt remote, or
// "" if the remote or database can't be found or the lookup fails.
func (d *Daemon) resolveRemoteURL(dataDir, db, remote string) string {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	query := fmt.Sprintf("USE `%s`; SELECT url FROM dolt_remotes WHERE name = '%s'", db, escapeSQL(remote))
	cmd := exec.CommandContext(ctx, "dolt", "sql", "-r", "csv", "-q", query)
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(lines[1])
}

// databaseHasRemote checks if a database has the specified remote configured.
func (d *Daemon) databaseHasRemote(dataDir, db, remote string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	query := fmt.Sprintf("USE `%s`; SELECT name FROM dolt_remotes WHERE name = '%s'", db, escapeSQL(remote))
	cmd := exec.CommandContext(ctx, "dolt", "sql", "-r", "csv", "-q", query)
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// If we get more than just the header line, the remote exists
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return len(lines) > 1
}

// databaseHasAnyRemote checks if a database has any remote configured.
func (d *Daemon) databaseHasAnyRemote(dataDir, db string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	query := fmt.Sprintf("USE `%s`; SELECT name FROM dolt_remotes LIMIT 1", db)
	cmd := exec.CommandContext(ctx, "dolt", "sql", "-r", "csv", "-q", query)
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return len(lines) > 1
}

// findDatabaseRemote returns the name of the first remote configured for a database.
// Returns empty string if no remote is found.
func (d *Daemon) findDatabaseRemote(dataDir, db string) string {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	query := fmt.Sprintf("USE `%s`; SELECT name FROM dolt_remotes LIMIT 1", db)
	cmd := exec.CommandContext(ctx, "dolt", "sql", "-r", "csv", "-q", query)
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(lines[1])
}

// discoverDatabasesWithAnyRemote lists databases that have any remote configured.
func (d *Daemon) discoverDatabasesWithAnyRemote(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("reading data dir: %w", err)
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		doltDir := filepath.Join(dataDir, name, ".dolt")
		if _, err := os.Stat(doltDir); os.IsNotExist(err) {
			continue
		}
		if d.databaseHasAnyRemote(dataDir, name) {
			databases = append(databases, name)
		}
	}

	return databases, nil
}
