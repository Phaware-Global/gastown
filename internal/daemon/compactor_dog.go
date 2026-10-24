package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/util"
)

// shortHash returns at most 8 characters of a hash for display.
func shortHash(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

const (
	defaultCompactorDogInterval = 24 * time.Hour
	// defaultCompactorCommitThreshold is the minimum commit count before compaction triggers.
	// 2000 commits prevents the escalation feedback loop where each compaction
	// failure creates beads/escalations that add more commits than the compactor
	// can drain at 500. Configurable via daemon.json.
	defaultCompactorCommitThreshold = 2000
	// compactorQueryTimeout is the timeout for individual SQL queries during compaction.
	compactorQueryTimeout = 30 * time.Second
	// compactorGCTimeout is the timeout for CALL dolt_gc() after compaction.
	compactorGCTimeout = 5 * time.Minute
	// compactorPushTimeout is the timeout for DOLT_PUSH after compaction.
	compactorPushTimeout = 2 * time.Minute
	// compactorBranchName is the temporary branch used during compaction.
	compactorBranchName = "gt-compaction"
	// surgicalMaxRetries is the number of times to retry surgical rebase after
	// a concurrent write error. DOLT_REBASE is NOT safe with concurrent writes —
	// Dolt detects that the commit graph changed and returns an error. One retry
	// is usually sufficient since the window for concurrent writes is small.
	surgicalMaxRetries = 1
)

// CompactorDogConfig holds configuration for the compactor_dog patrol.
type CompactorDogConfig struct {
	Enabled     bool     `json:"enabled"`
	IntervalStr string   `json:"interval,omitempty"`
	// Threshold is the minimum commit count before compaction triggers.
	// Defaults to 2000 if not set.
	Threshold int `json:"threshold,omitempty"`
	// Databases lists specific database names to compact.
	// If empty, falls back to wisp_reaper config, then auto-discovery.
	Databases []string `json:"databases,omitempty"`
	// Mode selects the compaction strategy: "flatten" (default) or "surgical".
	// Flatten squashes all history into 1 commit. Surgical keeps recent
	// commits individual while squashing old ones via interactive rebase.
	Mode string `json:"mode,omitempty"`
	// KeepRecent is the number of recent commits to preserve as individual
	// picks during surgical rebase. Only used when Mode is "surgical".
	// Defaults to 50 if not set.
	KeepRecent int `json:"keep_recent,omitempty"`
}

// compactorDogInterval returns the configured interval, or the default (24h).
func compactorDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.CompactorDog != nil {
		if config.Patrols.CompactorDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.CompactorDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultCompactorDogInterval
}

// compactorDogThreshold returns the configured commit threshold, or the default (500).
func compactorDogThreshold(config *DaemonPatrolConfig) int {
	if config != nil && config.Patrols != nil && config.Patrols.CompactorDog != nil {
		if config.Patrols.CompactorDog.Threshold > 0 {
			return config.Patrols.CompactorDog.Threshold
		}
	}
	return defaultCompactorCommitThreshold
}

// compactorDogMode returns the configured compaction mode ("flatten" or "surgical").
func compactorDogMode(config *DaemonPatrolConfig) string {
	if config != nil && config.Patrols != nil && config.Patrols.CompactorDog != nil {
		if config.Patrols.CompactorDog.Mode == "surgical" {
			return "surgical"
		}
	}
	return "flatten"
}

// compactorDogKeepRecent returns the configured keep-recent count, or the default (50).
func compactorDogKeepRecent(config *DaemonPatrolConfig) int {
	if config != nil && config.Patrols != nil && config.Patrols.CompactorDog != nil {
		if config.Patrols.CompactorDog.KeepRecent > 0 {
			return config.Patrols.CompactorDog.KeepRecent
		}
	}
	return 50
}

// runCompactorDog checks each production database's commit count and compacts
// any that exceed the threshold. Two modes:
//
// Flatten mode (default): soft-resets to root commit on main, commits all data
// as a single commit. Safe with concurrent writes.
//
// Surgical mode: interactive rebase that squashes old commits while preserving
// recent N as individual picks. NOT safe with concurrent writes — retries once.
//
// After successful compaction, runs dolt gc to reclaim unreferenced chunks.
//
// ZFC Exemption: This dog executes imperatively in Go rather than via agent-driven
// formula execution. The mol-dog-compactor formula is used for observability
// tracking only (pourDogMolecule + closeStep/failStep). Agent execution is
// impractical because: (1) operations require database/sql connections, (2)
// transactional state spans multiple queries, (3) cleanup-on-failure error paths,
// (4) concurrent write retry with error classification, (5) row count integrity
// verification. See mol-dog-compactor.formula.toml for full rationale.
func (d *Daemon) runCompactorDog() {
	if !d.isPatrolActive("compactor_dog") {
		return
	}

	threshold := compactorDogThreshold(d.patrolConfig)
	mode := compactorDogMode(d.patrolConfig)
	d.logger.Printf("compactor_dog: starting compaction cycle (threshold=%d, mode=%s)", threshold, mode)
	if mode == "surgical" {
		d.logger.Printf("compactor_dog: WARNING: surgical mode uses DOLT_REBASE which is not safe with concurrent writes — will retry on graph-change errors")
	}

	mol := d.pourDogMolecule(constants.MolDogCompactor, nil)
	defer mol.close()

	databases := d.compactorDatabases()
	if len(databases) == 0 {
		d.logger.Printf("compactor_dog: no databases to compact")
		mol.failStep("inspect", "no databases found")
		return
	}

	mol.closeStep("inspect")

	compacted := 0
	skipped := 0
	errors := 0

	for _, dbName := range databases {
		switch d.compactorProcessDatabase(dbName, threshold, mode) {
		case compactorCompacted:
			compacted++
		case compactorSkipped:
			skipped++
		case compactorErrored:
			errors++
		}
	}

	if errors > 0 {
		mol.failStep("compact", fmt.Sprintf("%d databases had errors", errors))
	} else {
		mol.closeStep("compact")
	}

	mol.closeStep("verify")

	d.logger.Printf("compactor_dog: cycle complete — compacted=%d skipped=%d errors=%d",
		compacted, skipped, errors)
	mol.closeStep("report")
}

// compactorDatabases returns the list of databases to consider for compaction.
// Checks its own config first, falls back to wisp_reaper config, then auto-discovery.
func (d *Daemon) compactorDatabases() []string {
	if d.patrolConfig != nil && d.patrolConfig.Patrols != nil {
		if cd := d.patrolConfig.Patrols.CompactorDog; cd != nil {
			if len(cd.Databases) > 0 {
				return cd.Databases
			}
		}
		if d.patrolConfig.Patrols.WispReaper != nil {
			if dbs := d.patrolConfig.Patrols.WispReaper.Databases; len(dbs) > 0 {
				return dbs
			}
		}
	}
	return reaper.DefaultDatabases
}

// compactorOutcome is what one compaction cycle did with one database.
type compactorOutcome int

const (
	compactorCompacted compactorOutcome = iota
	compactorSkipped
	compactorErrored
)

// compactorProcessDatabase runs one cycle for one database: threshold check,
// pre-flight against the remote, compaction, gc, push.
//
// INVARIANT: a database with a configured Dolt remote is never flattened in a
// cycle that cannot push to it. Flatten rewrites the commit graph, so a remote
// we could not push afterwards keeps an old HEAD that is no longer in local
// history; the next cycle that can reach it would read the database as
// diverged and skip it for good. So if the remote exists but the push guard
// refuses it for ANY reason (public, unknown, non-GitHub, DoltHub, unreadable
// URL, a failed or unavailable visibility lookup), the database is skipped for
// the cycle (no fetch, no flatten, no push) and escalated once per db and
// reason. A database with NO remote compacts locally. There is deliberately no
// per-reason branching. plugins/compactor-dog/run.sh enforces the same rule.
func (d *Daemon) compactorProcessDatabase(dbName string, threshold int, mode string) compactorOutcome {
	commitCount, err := d.compactorCountCommits(dbName)
	if err != nil {
		d.logger.Printf("compactor_dog: %s: error counting commits: %v", dbName, err)
		return compactorErrored
	}

	if commitCount < threshold {
		d.logger.Printf("compactor_dog: %s: %d commits (below threshold %d), skipping",
			dbName, commitCount, threshold)
		return compactorSkipped
	}

	d.logger.Printf("compactor_dog: %s: %d commits (threshold %d) — compacting (mode=%s)",
		dbName, commitCount, threshold, mode)

	// Pre-flight: decide the push target once (remote, URL, guard verdict),
	// then fetch from it and verify local ≥ remote before compacting.
	// Flatten rewrites the commit graph, so force-push after compaction
	// would overwrite any remote-only commits; skip compaction if the
	// remote has diverged.
	target, skip, fetchErr := d.compactorPreflightDB(dbName)
	if fetchErr != nil {
		d.logger.Printf("compactor_dog: %s: pre-flight fetch failed: %v (skipping)", dbName, fetchErr)
		return compactorSkipped
	}
	switch skip {
	case compactorSkipPushRefused:
		logLine, title, reason := compactorRefusalText(dbName, target)
		d.logger.Print(logLine)
		compactorEscalate(d, "compactor-dog:push-refused:"+dbName+":"+target.refusal, title, reason)
		return compactorSkipped
	case compactorSkipDiverged:
		d.logger.Printf("compactor_dog: %s: remote has diverged — skipping compaction to avoid data loss", dbName)
		return compactorSkipped
	}

	var compactErr error
	if mode == "surgical" {
		keepRecent := compactorDogKeepRecent(d.patrolConfig)
		compactErr = d.surgicalRebase(dbName, keepRecent)
	} else {
		compactErr = d.compactDatabase(dbName)
	}
	if compactErr != nil {
		d.logger.Printf("compactor_dog: %s: compaction FAILED: %v", dbName, compactErr)
		d.escalate("compactor_dog", fmt.Sprintf("Compaction failed for %s: %v", dbName, compactErr))
		return compactorErrored
	}

	// Run gc after successful compaction to reclaim unreferenced chunks.
	// Order matters: rebase first (compactDatabase), gc second.
	if err := d.compactorRunGC(dbName); err != nil {
		d.logger.Printf("compactor_dog: %s: gc after compaction failed: %v", dbName, err)
	}
	// Force-push to the remote after compaction, but only to the push
	// target decided in the pre-flight. Flatten rewrites the commit
	// graph, so standard push always fails with non-fast-forward.
	// Safe because the pre-flight confirmed local ≥ remote before
	// compaction, and compactDatabase verified integrity.
	if target.usable() {
		if err := d.compactorForcePush(dbName, target); err != nil {
			d.logger.Printf("compactor_dog: %s: force-push failed: %v", dbName, err)
		}
	}
	return compactorCompacted
}

// compactorRefusalText words the skip of a database whose push target was
// refused. A failed remote-name read leaves the remote's name unknown, so it
// says the configuration could not be read instead of presenting the lookup
// failure as the guard refusing a remote called "".
func compactorRefusalText(dbName string, target compactorPushTarget) (logLine, title, reason string) {
	if target.remote == "" {
		return fmt.Sprintf("compactor_dog: %s: could not read the Dolt remote configuration (%s) — skipping compaction this cycle (no fetch, no flatten, no push)", dbName, target.refusal),
			fmt.Sprintf("compactor_dog: skipped %s — could not read its Dolt remote configuration (%s)", dbName, target.refusal),
			fmt.Sprintf("compactor-dog could not read the Dolt remote configuration of %s (%s), so it cannot tell whether a push is possible and skipped compaction: flattening a database whose remote it cannot push would leave that remote diverged. Check that the Dolt server is healthy; the skip repeats until the read succeeds.", dbName, target.refusal)
	}
	return fmt.Sprintf("compactor_dog: %s: REFUSED: remote %q cannot be pushed (%s) — skipping compaction this cycle (no fetch, no flatten, no push)", dbName, target.remote, target.refusal),
		fmt.Sprintf("compactor_dog: skipped %s — its Dolt remote cannot be pushed (%s)", dbName, target.refusal),
		fmt.Sprintf("%s has a Dolt remote %q that the push visibility guard refused (%s), so compaction was skipped: flattening without being able to push would leave the remote diverged. Make the remote a private GitHub repo or remove it, or fix the guard environment (gh).", dbName, target.remote, target.refusal)
}

// compactorEscalate raises a deduplicated escalation: gt escalate suppresses a
// repeat while one with the same fingerprint is still open. A variable so
// tests can capture escalations instead of running gt.
var compactorEscalate = func(d *Daemon, fingerprint, title, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gt", "escalate", title, "-s", "MEDIUM",
		"--fingerprint", fingerprint, "--reason", reason)
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("compactor_dog: escalation failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}
}

// compactorCountCommits counts the number of commits in the database's dolt_log.
func (d *Daemon) compactorCountCommits(dbName string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("count dolt_log: %w", err)
	}
	return count, nil
}

// compactDatabase performs the full flatten operation on a single database.
// Uses direct SQL on the running server — no branches, no downtime.
// Per Tim Sehn (2026-02-28): DOLT_RESET --soft + DOLT_COMMIT is safe on a
// running server. Concurrent writes are safe — merge base shifts but diff
// is just the txn.
func (d *Daemon) compactDatabase(dbName string) error {
	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	// Step 1: Record pre-flight state — row counts for integrity verification.
	preCounts, err := d.compactorGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("pre-flight row counts: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: pre-flight tables=%d", dbName, len(preCounts))

	// Step 2: Find the root commit (earliest in history).
	rootHash, err := d.compactorGetRootCommit(db, dbName)
	if err != nil {
		return fmt.Errorf("find root commit: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: root commit=%s", dbName, shortHash(rootHash))

	// Step 3: USE database for session-scoped operations.
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}

	// Step 4: Soft-reset to root commit on main — all data remains staged.
	// This is trivially cheap: just moves the parent pointer (Tim Sehn).
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--soft', '%s')", rootHash)); err != nil {
		return fmt.Errorf("soft reset to root: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: soft-reset to root %s", dbName, shortHash(rootHash))

	// Step 5: Commit all data as a single commit.
	commitMsg := fmt.Sprintf("compaction: flatten %s history to single commit", dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil {
		return fmt.Errorf("commit flattened data: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: committed flattened data", dbName)

	// Step 6: Verify integrity — row counts must not decrease (data loss).
	// Concurrent writes may increase counts during compaction — this is safe
	// since the flattened commit includes those rows.
	postCounts, err := d.compactorGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("post-compact row counts: %w", err)
	}

	for table, preCount := range preCounts {
		postCount, ok := postCounts[table]
		if !ok {
			return fmt.Errorf("integrity check: table %q missing after compaction", table)
		}
		if postCount < preCount {
			return fmt.Errorf("integrity check: table %q lost rows: pre=%d post=%d", table, preCount, postCount)
		}
		if postCount > preCount {
			d.logger.Printf("compactor_dog: %s: table %q gained %d rows during compaction (concurrent write, safe)",
				dbName, table, postCount-preCount)
		}
	}
	d.logger.Printf("compactor_dog: %s: integrity verified (%d tables match)", dbName, len(preCounts))

	// Step 7: Verify final commit count.
	finalCount, err := d.compactorCountCommits(dbName)
	if err != nil {
		d.logger.Printf("compactor_dog: %s: warning: could not verify final commit count: %v", dbName, err)
	} else {
		d.logger.Printf("compactor_dog: %s: compaction complete — %d commits remain", dbName, finalCount)
	}

	return nil
}

// surgicalRebase performs interactive rebase on a single database:
// squashes old commits while keeping the most recent N as individual picks.
// This is an alternative to the flatten algorithm that preserves recent history.
//
// CONCURRENT WRITE HAZARD: Unlike flatten mode (which uses DOLT_RESET --soft
// and is safe with concurrent writes), surgical mode uses DOLT_REBASE which is
// NOT safe with concurrent writes. If an agent commits to the database while a
// rebase is in progress, Dolt detects the graph change and returns an error.
// This function retries once on such errors, which is usually sufficient since
// the collision window is small. If retries are exhausted, the error is returned
// to the caller for escalation.
// Ref: Tim Sehn (2026-02-28) confirmed DOLT_REBASE fails on concurrent writes.
func (d *Daemon) surgicalRebase(dbName string, keepRecent int) error {
	var lastErr error
	for attempt := 0; attempt <= surgicalMaxRetries; attempt++ {
		if attempt > 0 {
			d.logger.Printf("compactor_dog: %s: surgical rebase retry %d/%d after concurrent write error",
				dbName, attempt, surgicalMaxRetries)
			// Brief pause before retry to let the concurrent write finish.
			time.Sleep(2 * time.Second)
		}
		lastErr = d.surgicalRebaseOnce(dbName, keepRecent)
		if lastErr == nil {
			return nil
		}
		if !isConcurrentWriteError(lastErr) {
			return lastErr
		}
		d.logger.Printf("compactor_dog: %s: concurrent write detected during surgical rebase: %v", dbName, lastErr)
	}
	return fmt.Errorf("surgical rebase failed after %d retries due to concurrent writes: %w", surgicalMaxRetries, lastErr)
}

// isConcurrentWriteError returns true if the error indicates Dolt detected a
// concurrent write during rebase (commit graph changed underneath the operation).
func isConcurrentWriteError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Dolt reports graph-change errors during rebase when concurrent writes occur.
	// Also catch our own concurrency abort from HEAD movement detection.
	return strings.Contains(msg, "rebase execution failed") ||
		strings.Contains(msg, "concurrency abort") ||
		strings.Contains(msg, "graph") ||
		strings.Contains(msg, "cannot rebase")
}

// surgicalRebaseOnce performs a single attempt at surgical rebase.
func (d *Daemon) surgicalRebaseOnce(dbName string, keepRecent int) error {
	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	// Pre-flight: record state.
	preHead, err := d.compactorGetHead(db, dbName)
	if err != nil {
		return fmt.Errorf("pre-flight HEAD: %w", err)
	}
	preCounts, err := d.compactorGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("pre-flight row counts: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: surgical rebase pre-flight HEAD=%s, tables=%d, keep_recent=%d",
		dbName, shortHash(preHead), len(preCounts), keepRecent)

	rootHash, err := d.compactorGetRootCommit(db, dbName)
	if err != nil {
		return fmt.Errorf("find root commit: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}

	const baseBranch = "compact-base"
	const workBranch = "compact-work"

	// Clean up leftover branches.
	d.surgicalCleanup(db, baseBranch, workBranch)

	// Step 1: Create anchor branch at root commit.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('%s', '%s')", baseBranch, rootHash)); err != nil {
		return fmt.Errorf("create base branch: %w", err)
	}

	// Step 2: Create work branch from main.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('%s', 'main')", workBranch)); err != nil {
		d.surgicalCleanupBase(db, baseBranch)
		return fmt.Errorf("create work branch: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHECKOUT('%s')", workBranch)); err != nil {
		d.surgicalCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("checkout work branch: %w", err)
	}

	// Step 3: Start interactive rebase.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_REBASE('--interactive', '%s')", baseBranch)); err != nil {
		d.surgicalCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("start interactive rebase: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: interactive rebase started", dbName)

	// Step 4: Read rebase plan bounds and mark old commits as squash.
	// Dolt returns rebase_order as DECIMAL — the MySQL driver delivers it as
	// []uint8 (e.g. "1.00") which cannot be scanned directly into int.
	var minOrderStr, maxOrderStr string
	if err := db.QueryRowContext(ctx, "SELECT MIN(rebase_order), MAX(rebase_order) FROM dolt_rebase").Scan(&minOrderStr, &maxOrderStr); err != nil {
		d.surgicalAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("read rebase bounds: %w", err)
	}
	minOrder, maxOrder, err := parseRebaseOrder2(minOrderStr, maxOrderStr)
	if err != nil {
		d.surgicalAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("parse rebase bounds: %w", err)
	}

	squashThreshold := maxOrder - keepRecent
	if squashThreshold <= minOrder {
		d.logger.Printf("compactor_dog: %s: nothing to squash (all commits recent), aborting rebase", dbName)
		d.surgicalAbortAndCleanup(db, baseBranch, workBranch)
		return nil
	}

	result, err := db.ExecContext(ctx, fmt.Sprintf(
		"UPDATE dolt_rebase SET action = 'squash' WHERE rebase_order > %d AND rebase_order <= %d",
		minOrder, squashThreshold))
	if err != nil {
		d.surgicalAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("update rebase plan: %w", err)
	}
	affected, _ := result.RowsAffected()
	d.logger.Printf("compactor_dog: %s: marked %d commits as squash", dbName, affected)

	// Step 5: Execute the rebase.
	if _, err := db.ExecContext(ctx, "CALL DOLT_REBASE('--continue')"); err != nil {
		d.surgicalCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("rebase execution failed: %w", err)
	}
	d.logger.Printf("compactor_dog: %s: rebase executed successfully", dbName)

	// Step 6: Verify integrity — row counts must not decrease (data loss).
	// Concurrent writes may increase counts during rebase — this is safe.
	postCounts, err := d.compactorGetRowCounts(db, dbName)
	if err != nil {
		d.logger.Printf("compactor_dog: %s: WARNING: could not verify row counts after rebase: %v", dbName, err)
	} else {
		for table, preCount := range preCounts {
			postCount, ok := postCounts[table]
			if !ok {
				d.surgicalCleanup(db, baseBranch, workBranch)
				return fmt.Errorf("integrity: table %q missing after rebase", table)
			}
			if postCount < preCount {
				d.surgicalCleanup(db, baseBranch, workBranch)
				return fmt.Errorf("integrity: table %q lost rows: pre=%d post=%d", table, preCount, postCount)
			}
			if postCount > preCount {
				d.logger.Printf("compactor_dog: %s: table %q gained %d rows during rebase (concurrent write, safe)",
					dbName, table, postCount-preCount)
			}
		}
		d.logger.Printf("compactor_dog: %s: integrity verified (%d tables)", dbName, len(preCounts))
	}

	// Step 7: Concurrency check.
	currentHead, err := d.compactorGetHead(db, dbName)
	if err != nil {
		d.surgicalCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("concurrency check: %w", err)
	}
	if currentHead != preHead {
		d.surgicalCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("concurrency abort: main HEAD moved from %s to %s", shortHash(preHead), shortHash(currentHead))
	}

	// Step 8: Swap branches — make compact-work the new main.
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', 'main')"); err != nil {
		return fmt.Errorf("delete old main: %w (compact-work preserved for recovery)", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-m', '%s', 'main')", workBranch)); err != nil {
		return fmt.Errorf("rename work to main: %w", err)
	}
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return fmt.Errorf("checkout new main: %w", err)
	}

	finalCount, _ := d.compactorCountCommits(dbName)
	d.logger.Printf("compactor_dog: %s: surgical rebase complete — %d commits remain", dbName, finalCount)
	return nil
}

// surgicalCleanup switches back to main and removes rebase branches.
//nolint:unparam // baseBranch always "compact-base" — API kept flexible for future callers
func (d *Daemon) surgicalCleanup(db *sql.DB, baseBranch, workBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// surgicalAbortAndCleanup aborts an in-progress rebase, then cleans up.
func (d *Daemon) surgicalAbortAndCleanup(db *sql.DB, baseBranch, workBranch string) { //nolint:unparam // baseBranch is always "compact-base" but kept for API symmetry with surgicalCleanup
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()
	_, _ = db.ExecContext(ctx, "CALL DOLT_REBASE('--abort')")
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// parseRebaseOrder2 parses min/max rebase_order DECIMAL strings to ints.
// Dolt's dolt_rebase table returns rebase_order as DECIMAL which the MySQL
// driver delivers as []uint8 (e.g. "1.00"), not directly scannable to int.
func parseRebaseOrder2(minStr, maxStr string) (int, int, error) {
	minF, err := strconv.ParseFloat(minStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid min rebase_order %q: %w", minStr, err)
	}
	maxF, err := strconv.ParseFloat(maxStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid max rebase_order %q: %w", maxStr, err)
	}
	return int(math.Round(minF)), int(math.Round(maxF)), nil
}

// surgicalCleanupBase removes only the base branch (work branch not yet created).
func (d *Daemon) surgicalCleanupBase(db *sql.DB, baseBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// compactorCleanup attempts to switch back to main and delete the temp branch.
// Called on error during compaction to leave the database in a clean state.
func (d *Daemon) compactorCleanup(db *sql.DB, dbName string) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	d.logger.Printf("compactor_dog: %s: cleaning up compaction branch", dbName)

	// Try to get back to main.
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	// Delete the compaction branch.
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", compactorBranchName))
}

// compactorOpenDriver is the database/sql driver compactorOpenDB uses; a
// variable so tests can substitute a fake server.
var compactorOpenDriver = "mysql"

// compactorOpenDB opens a connection to the Dolt server for the given database.
func (d *Daemon) compactorOpenDB(dbName string) (*sql.DB, error) {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		"127.0.0.1", d.doltServerPort(), dbName)
	return sql.Open(compactorOpenDriver, dsn)
}

// compactorGetHead returns the current HEAD commit hash of the main branch.
func (d *Daemon) compactorGetHead(db *sql.DB, dbName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	var hash string
	query := fmt.Sprintf("SELECT DOLT_HASHOF('main') FROM `%s`.dual", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&hash); err != nil {
		// Fallback: try without dual table.
		query = fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date DESC LIMIT 1", dbName)
		if err := db.QueryRowContext(ctx, query).Scan(&hash); err != nil {
			return "", err
		}
	}
	return hash, nil
}

// compactorGetRootCommit returns the hash of the earliest commit in the database.
func (d *Daemon) compactorGetRootCommit(db *sql.DB, dbName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	var hash string
	query := fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&hash); err != nil {
		return "", err
	}
	return hash, nil
}

// compactorGetRowCounts returns a map of table -> row count for all user tables.
func (d *Daemon) compactorGetRowCounts(db *sql.DB, dbName string) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	// Get list of user tables (excluding dolt system tables).
	query := fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema = '%s' AND table_name NOT LIKE 'dolt_%%'", dbName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}

	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		countQuery := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s`", dbName, table)
		if err := db.QueryRowContext(ctx, countQuery).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = count
	}

	return counts, nil
}

// compactorRunGC runs dolt gc via SQL on the running server after compaction.
// GC reclaims unreferenced chunks left behind by the flatten operation.
// Auto-GC is on by default since Dolt 1.75.0 (triggers at 50MB journal),
// but we run it explicitly after compaction for immediate cleanup.
func (d *Daemon) compactorRunGC(dbName string) error {
	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), compactorGCTimeout)
	defer cancel()

	start := time.Now()
	if _, err := db.ExecContext(ctx, "CALL dolt_gc()"); err != nil {
		elapsed := time.Since(start)
		if ctx.Err() == context.DeadlineExceeded {
			d.logger.Printf("compactor_dog: gc: %s: TIMEOUT after %v", dbName, elapsed)
			return fmt.Errorf("gc timeout after %v", elapsed)
		}
		d.logger.Printf("compactor_dog: gc: %s: failed after %v: %v", dbName, elapsed, err)
		return fmt.Errorf("dolt_gc: %w", err)
	}

	d.logger.Printf("compactor_dog: gc: %s: completed in %v", dbName, time.Since(start))
	return nil
}

// compactorPushTarget is a database's push destination, decided once per
// cycle by compactorResolvePushTarget before anything touches the remote.
// The zero value means "no remote". A remote the guard refused keeps its name
// and URL (for the log) and carries the refusal reason; it is not usable, so
// it is never fetched, diverged-checked or pushed.
type compactorPushTarget struct {
	remote  string
	url     string
	refusal string // "" = not refused; otherwise the guard's reason
}

// usable reports whether the remote may be fetched from and pushed to.
func (t compactorPushTarget) usable() bool { return t.remote != "" && t.refusal == "" }

// compactorResolvePushTarget decides the push target for a database: its
// remote name, URL and the remoteguard verdict, in one place. The database
// holds issues and mail, so only a confirmed-private GitHub repo is a target;
// an unreadable URL is refused like a public one. A database with no remote
// (or a failed remote lookup) has no target and nothing to refuse.
func (d *Daemon) compactorResolvePushTarget(db *sql.DB) compactorPushTarget {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	// Every read here tells "failed" from "empty". Only a successful lookup
	// that finds no remote means "no remote"; a failed lookup leaves the
	// destination unknown, which is a refusal, not an absence.
	var remoteName string
	if err := db.QueryRowContext(ctx, "SELECT name FROM dolt_remotes LIMIT 1").Scan(&remoteName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return compactorPushTarget{}
		}
		return compactorPushTarget{refusal: "remote-lookup-failed"}
	}
	if remoteName == "" {
		return compactorPushTarget{}
	}

	var remoteURL string
	if err := db.QueryRowContext(ctx, "SELECT url FROM dolt_remotes WHERE name = ?", remoteName).Scan(&remoteURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return compactorPushTarget{remote: remoteName, refusal: "url-unresolvable"}
		}
		return compactorPushTarget{remote: remoteName, refusal: "remote-lookup-failed"}
	}
	if remoteURL == "" {
		return compactorPushTarget{remote: remoteName, refusal: "url-unresolvable"}
	}
	if allowed, reason := remotePushAllowed(ctx, remoteURL); !allowed {
		return compactorPushTarget{remote: remoteName, url: remoteURL, refusal: reason}
	}
	return compactorPushTarget{remote: remoteName, url: remoteURL}
}

// compactorPreflightDB is compactorPreflight on a fresh connection.
func (d *Daemon) compactorPreflightDB(dbName string) (compactorPushTarget, compactorSkip, error) {
	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return compactorPushTarget{}, "", err
	}
	defer db.Close()
	return d.compactorPreflight(db, dbName)
}

// compactorSkip says why a database is not compacted this cycle; "" = go ahead.
type compactorSkip string

const (
	// compactorSkipPushRefused: the database has a remote the guard refused.
	compactorSkipPushRefused compactorSkip = "push-refused"
	// compactorSkipDiverged: the (allowed) remote has commits local lacks.
	compactorSkipDiverged compactorSkip = "remote-diverged"
)

// compactorPreflight resolves the database's push target, then — only for a
// usable target — fetches it and checks that the local history contains the
// remote HEAD. A refused remote is never fetched and skips the database
// (compactorSkipPushRefused); a remote with commits local lacks skips it too
// (compactorSkipDiverged). The returned target is what the cycle's force-push
// must use.
func (d *Daemon) compactorPreflight(db *sql.DB, dbName string) (target compactorPushTarget, skip compactorSkip, err error) {
	target = d.compactorResolvePushTarget(db)
	if target.refusal != "" {
		return target, compactorSkipPushRefused, nil
	}
	if !target.usable() {
		return target, "", nil // No remote — compacts locally
	}
	diverged, err := d.compactorFetchAndVerify(db, dbName, target)
	if err != nil {
		return target, "", err
	}
	if diverged {
		return target, compactorSkipDiverged, nil
	}
	return target, "", nil
}

// compactorFetchAndVerify fetches the push target and checks that the local
// history contains the remote HEAD. Returns (diverged=true, nil) if the remote
// has commits not in local history — compaction must be skipped to avoid data
// loss on force-push. Returns (false, nil) if local ≥ remote.
// Mirrors the shell script's pre-flight check (run.sh).
func (d *Daemon) compactorFetchAndVerify(db *sql.DB, dbName string, target compactorPushTarget) (diverged bool, err error) {
	if !target.usable() {
		return false, fmt.Errorf("refusing to fetch %q for %s: not a usable push target", target.remote, dbName)
	}
	remoteName := target.remote

	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	// Fetch from remote.
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), compactorPushTimeout)
	defer fetchCancel()
	if _, err := db.ExecContext(fetchCtx, "CALL DOLT_FETCH(?)", remoteName); err != nil {
		return false, fmt.Errorf("DOLT_FETCH %s: %w", remoteName, err)
	}

	// Get remote HEAD. Dolt's dolt_remote_branches has columns (name, hash,
	// ...) and names a tracking ref "remotes/<remote>/<branch>".
	var remoteHead string
	remoteRef := "remotes/" + remoteName + "/main"
	err = db.QueryRowContext(ctx,
		"SELECT hash FROM dolt_remote_branches WHERE name = ?", remoteRef,
	).Scan(&remoteHead)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // Remote has no main branch yet — nothing to diverge from
		}
		// The check could not be made: do not treat that as "no divergence".
		return false, fmt.Errorf("remote branch lookup %s: %w", remoteRef, err)
	}

	// Check if remote HEAD is an ancestor of local history.
	var isAncestor int
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?", remoteHead,
	).Scan(&isAncestor)
	if err != nil {
		return false, fmt.Errorf("ancestor check: %w", err)
	}

	if isAncestor == 0 {
		return true, nil // Diverged — remote has commits we don't have
	}

	d.logger.Printf("compactor_dog: fetch: %s: local ≥ %s (verified)", dbName, remoteName)
	return false, nil
}

// compactorForcePush pushes the compacted database to its push target.
// Flatten rewrites the commit graph, so a force-push is required — standard
// push always fails with non-fast-forward. This mirrors the shell script's
// DOLT_PUSH('--force', '--set-upstream', remote, 'main') in
// plugins/compactor-dog/run.sh: flatten loses the branch's tracking metadata,
// so without --set-upstream and an explicit branch Dolt refuses the push ("The
// current branch main has no upstream branch").
// Non-fatal: push failures don't affect local data integrity.
func (d *Daemon) compactorForcePush(dbName string, target compactorPushTarget) error {
	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return err
	}
	defer db.Close()
	return d.compactorForcePushDB(db, dbName, target)
}

// compactorForcePushDB is compactorForcePush on an open connection. It pushes
// only to a usable target; no remote is a quiet no-op, and a refused target is
// an error, so a caller that skipped the pre-flight cannot push anyway.
func (d *Daemon) compactorForcePushDB(db *sql.DB, dbName string, target compactorPushTarget) error {
	if !target.usable() && target.refusal == "" {
		return nil // No remote configured — skip silently
	}
	if !target.usable() {
		return fmt.Errorf("REFUSED: %q: remote %q is not a confirmed-private GitHub repo (%s)", dbName, target.remote, target.refusal)
	}
	remoteName := target.remote

	// Force-push to remote.
	pushCtx, pushCancel := context.WithTimeout(context.Background(), compactorPushTimeout)
	defer pushCancel()

	start := time.Now()
	if _, err := db.ExecContext(pushCtx, "CALL DOLT_PUSH('--force', '--set-upstream', ?, 'main')", remoteName); err != nil {
		return fmt.Errorf("DOLT_PUSH --force to %s: %w", remoteName, err)
	}

	d.logger.Printf("compactor_dog: push: %s → %s: force-pushed in %v", dbName, remoteName, time.Since(start))
	return nil
}
