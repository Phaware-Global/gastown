//go:build integration

package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/testutil"
)

// requireDoltServer delegates to testutil.RequireDoltContainer.
func requireDoltServer(t *testing.T) {
	t.Helper()
	testutil.RequireDoltContainer(t)
}

// cleanupDoltServer delegates to testutil.TerminateDoltContainer.
func cleanupDoltServer() {
	testutil.TerminateDoltContainer()
}

// configureTestGitIdentity sets git global config in an isolated HOME directory
// so that EnsureDoltIdentity (called during gt install preflight) can copy
// identity from git to dolt.
func configureTestGitIdentity(t *testing.T, homeDir string) {
	t.Helper()
	env := append(os.Environ(), "HOME="+homeDir)
	for _, args := range [][]string{
		{"config", "--global", "user.name", "Test User"},
		{"config", "--global", "user.email", "test@test.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

// bridgeDoltPidToTown writes the Go test process PID into townRoot/daemon/dolt.pid
// so that doltserver.IsRunning(townRoot) finds it via the PID-file shortcut path.
//
// With containers there is no dolt binary PID file. We write our own process PID
// instead — the PID file's purpose is to make IsRunning() take the PID-file
// shortcut path (which just checks if the PID is alive, not the process name).
func bridgeDoltPidToTown(t *testing.T, townRoot string) {
	t.Helper()

	pid := fmt.Sprintf("%d", os.Getpid())

	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("bridgeDoltPidToTown: mkdir daemon: %v", err)
	}
	townPidPath := filepath.Join(daemonDir, "dolt.pid")
	if err := os.WriteFile(townPidPath, []byte(pid+"\n"), 0644); err != nil { //nolint:gosec
		t.Fatalf("bridgeDoltPidToTown: write PID file: %v", err)
	}
}

// doltCleanupOnce ensures database cleanup happens at most once per binary.
var (
	doltCleanupOnce sync.Once
	doltCleanupErr  error
)

// cleanStaleBeadsDatabases drops stale beads_* databases left by earlier tests
// (e.g., beads_db_init_test.go) from the running Dolt server. This prevents
// phantom catalog entries from causing "database not found" errors during
// bd init --server migration sweeps in queue tests.
//
// Uses SQL-level cleanup (DROP DATABASE) rather than server restart, because
// restarting the Dolt server causes bd init --server to fail at creating
// database schema (tables).
func cleanStaleBeadsDatabases(t *testing.T) {
	t.Helper()
	doltCleanupOnce.Do(func() {
		doltCleanupErr = dropStaleBeadsDatabases()
	})
	if doltCleanupErr != nil {
		t.Fatalf("stale database cleanup failed: %v", doltCleanupErr)
	}
}

// dropStaleBeadsDatabases connects to the Dolt server and drops all beads_*
// databases that were created by earlier tests. Uses two strategies:
//  1. SHOW DATABASES → DROP any visible beads_* databases
//  2. DROP known phantom database names from beads_db_init_test.go
func dropStaleBeadsDatabases() error {
	dsn := "root:@tcp(127.0.0.1:" + testutil.DoltContainerPort() + ")/"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connecting to dolt server: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("pinging dolt server: %w", err)
	}

	var dropped []string

	// Strategy 1: Drop beads_* and known test databases (not ALL non-system databases,
	// to avoid destroying unrelated integration state on shared servers).
	systemDBs := map[string]bool{
		"information_schema": true,
		"mysql":              true,
	}
	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[dropStaleBeadsDatabases] SHOW DATABASES failed: %v\n", err)
	} else {
		var allDBs []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				continue
			}
			allDBs = append(allDBs, name)
			// Only drop databases matching known test patterns
			shouldDrop := false
			if strings.HasPrefix(name, "beads_") {
				shouldDrop = true
			} else if name == "hq" {
				shouldDrop = true // Created by beads_db_init_test.go
			}
			if shouldDrop && !systemDBs[name] {
				if _, err := db.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
					fmt.Fprintf(os.Stderr, "[dropStaleBeadsDatabases] DROP %s failed: %v\n", name, err)
				} else {
					dropped = append(dropped, name)
				}
			}
		}
		rows.Close()
		fmt.Fprintf(os.Stderr, "[dropStaleBeadsDatabases] visible databases: %v\n", allDBs)
	}

	// Strategy 2: Try to DROP known phantom database names from beads_db_init_test.go.
	// These may be invisible to SHOW DATABASES but still in Dolt's in-memory catalog.
	knownPrefixes := []string{
		"existing-prefix", "empty-prefix", "real-prefix",
		"original-prefix", "reinit-prefix",
		"myrig", "emptyrig", "mismatchrig", "testrig", "reinitrig",
		"prefix-test", "no-issues-test", "mismatch-test", "derived-test", "reinit-test",
	}
	for _, pfx := range knownPrefixes {
		name := "beads_" + pfx
		if _, err := db.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			fmt.Fprintf(os.Stderr, "[dropStaleBeadsDatabases] DROP phantom %s: %v\n", name, err)
		} else {
			dropped = append(dropped, name+"(phantom)")
		}
	}

	// Strategy 3: Purge dropped databases from Dolt's catalog.
	if _, err := db.Exec("CALL dolt_purge_dropped_databases()"); err != nil {
		fmt.Fprintf(os.Stderr, "[dropStaleBeadsDatabases] purge failed: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "[dropStaleBeadsDatabases] cleaned: %v\n", dropped)
	return nil
}

// stopTownProcesses terminates every process whose command line mentions root
// (the test town's unique temp path) and WAITS for them to actually exit.
//
// The waiting is the point. These tests register a t.Cleanup that killed the
// town's managed Dolt with a bare `pkill -f <root>`, but pkill only delivers
// the signal — it returns as soon as the signal is sent, not when the process
// has exited and closed its files. Go then runs the parent test's t.TempDir()
// teardown, which RemoveAll's that same directory while Dolt is still shutting
// down and still holding files inside it. The removal fails with
//
//	TempDir RemoveAll cleanup: unlinkat /tmp/TestX/001: directory not empty
//
// and the whole test fails even though every subtest passed. That is a race,
// so it surfaces intermittently under load — it has been failing
// TestBeadsDbInitAfterClone on CI and in the nightly integration run.
//
// SIGTERM first so Dolt can close its files cleanly, then escalate to SIGKILL
// if it outstays the grace period; a killed-but-unreaped Dolt would leave the
// same directory unremovable. Failures are deliberately silent: pkill exits
// non-zero when nothing matched, which is the normal case for a town that
// never started a server, and a cleanup helper must not fail a passing test.
func stopTownProcesses(t *testing.T, root string) {
	t.Helper()
	if strings.TrimSpace(root) == "" {
		return
	}
	_ = exec.Command("pkill", "-f", root).Run()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", root).Run() != nil {
			return // pgrep exits non-zero when nothing matches: all gone.
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still alive after the grace period. SIGKILL, then give the kernel a
	// moment to reap it and release the file handles.
	_ = exec.Command("pkill", "-9", "-f", root).Run()
	hardDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(hardDeadline) {
		if exec.Command("pgrep", "-f", root).Run() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
