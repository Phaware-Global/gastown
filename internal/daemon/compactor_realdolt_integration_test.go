//go:build integration

package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real-Dolt tests for the compactor's push-target and divergence path.
//
// The fakes in compactor_force_push_guard_test.go answer whatever SQL the code
// sends and so cannot notice when that SQL does not match Dolt (rounds 3-5 of
// the review all missed a query naming a column, commit_hash, that
// dolt_remote_branches does not have, and a ref name, origin/main, Dolt does
// not use). These tests run the real dolt binary: a throwaway sql-server on a
// free loopback port over a temp data dir, with file:// remotes in temp dirs.
// They never touch the town's Dolt server (port 3307) and never push anywhere
// off this machine. Skipped when the dolt binary is not installed; CI's
// integration job installs it.

const realDoltTimeout = 60 * time.Second

func requireDoltBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not installed")
	}
}

// doltIn runs the dolt CLI in dir with an isolated DOLT_ROOT_PATH.
func doltIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("dolt", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ() // DOLT_ROOT_PATH set by the caller via t.Setenv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// realDoltFixture describes a database prepared on disk before its server starts.
type realDoltFixture struct {
	commits     int  // commits on main after the initial one
	remote      bool // add a file:// remote named origin and push main to it
	remoteAhead bool // then add a commit on the remote that the local database lacks
}

type realDolt struct {
	dataDir   string
	port      int
	remoteDir string // file:// remote's directory ("" without one)
}

// startRealDolt prepares database "gt" per fx, then starts a real sql-server
// over it on a free loopback port.
func startRealDolt(t *testing.T, fx realDoltFixture) *realDolt {
	t.Helper()
	requireDoltBinary(t)

	t.Setenv("DOLT_ROOT_PATH", t.TempDir())
	doltIn(t, t.TempDir(), "config", "--global", "--add", "user.name", "compactor-test")
	doltIn(t, t.TempDir(), "config", "--global", "--add", "user.email", "compactor-test@example.com")

	rd := &realDolt{dataDir: t.TempDir()}
	dbDir := filepath.Join(rd.dataDir, "gt")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doltIn(t, dbDir, "init")
	doltIn(t, dbDir, "sql", "-q", "CREATE TABLE issues (id INT PRIMARY KEY, title VARCHAR(100))")
	doltIn(t, dbDir, "add", ".")
	doltIn(t, dbDir, "commit", "-m", "create issues")
	for i := 1; i <= fx.commits; i++ {
		doltIn(t, dbDir, "sql", "-q", fmt.Sprintf("INSERT INTO issues VALUES (%d, 'issue %d')", i, i))
		doltIn(t, dbDir, "add", ".")
		doltIn(t, dbDir, "commit", "-m", fmt.Sprintf("issue %d", i))
	}

	if fx.remote {
		rd.remoteDir = t.TempDir()
		remoteURL := "file://" + rd.remoteDir
		doltIn(t, dbDir, "remote", "add", "origin", remoteURL)
		doltIn(t, dbDir, "push", "origin", "main")
		if fx.remoteAhead {
			other := t.TempDir()
			doltIn(t, other, "clone", remoteURL, "other")
			clone := filepath.Join(other, "other")
			doltIn(t, clone, "sql", "-q", "INSERT INTO issues VALUES (9999, 'only on the remote')")
			doltIn(t, clone, "add", ".")
			doltIn(t, clone, "commit", "-m", "remote-only commit")
			doltIn(t, clone, "push", "origin", "main")
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rd.port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if rd.port == 3307 {
		t.Fatal("refusing to use the town Dolt server's port")
	}

	cmd := exec.Command("dolt", "sql-server", "--host", "127.0.0.1", "--port", fmt.Sprint(rd.port))
	cmd.Dir = rd.dataDir
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(realDoltTimeout)
	for {
		db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/gt?timeout=2s", rd.port))
		if err == nil {
			err = db.Ping()
			_ = db.Close()
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not come up on port %d: %v", rd.port, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return rd
}

func (rd *realDolt) daemon(t *testing.T) *Daemon {
	return &Daemon{
		config:     &Config{TownRoot: filepath.Dir(rd.dataDir)},
		logger:     log.New(testLogWriter{t}, "", 0),
		doltServer: &DoltServerManager{config: &DoltServerConfig{Port: rd.port}},
	}
}

func (rd *realDolt) open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/gt?timeout=5s&readTimeout=60s&writeTimeout=60s", rd.port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (rd *realDolt) scalar(t *testing.T, query string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realDoltTimeout)
	defer cancel()
	var v sql.NullString
	if err := rd.open(t).QueryRowContext(ctx, query).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v.String
}

func (rd *realDolt) commitCount(t *testing.T) int {
	t.Helper()
	var n int
	fmt.Sscan(rd.scalar(t, "SELECT COUNT(*) FROM dolt_log"), &n)
	return n
}

// remoteTrackingHash is the recorded HEAD of origin/main, or "" if none.
func (rd *realDolt) remoteTrackingHash(t *testing.T) string {
	t.Helper()
	return rd.scalar(t, "SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'")
}

const realDoltThreshold = 1 // every fixture database exceeds it

// No remote: nothing to push, so the database compacts locally.
func TestRealDolt_NoRemoteCompactsLocally(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 4})
	before := rd.commitCount(t)
	esc := recordCompactorEscalations(t)

	if got := rd.daemon(t).compactorProcessDatabase("gt", realDoltThreshold, "flatten"); got != compactorCompacted {
		t.Fatalf("outcome = %v, want compacted", got)
	}
	if after := rd.commitCount(t); after >= before {
		t.Errorf("history was not flattened: %d -> %d commits", before, after)
	}
	if n := len(esc.all()); n != 0 {
		t.Errorf("no remote must not escalate, got %d", n)
	}
}

// An allowed remote that is in sync: fetched, divergence check passes against
// Dolt's real dolt_remote_branches, compacted, and force-pushed so the remote
// ends up at the new local HEAD.
func TestRealDolt_AllowedRemoteInSyncCompactsAndPushes(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 4, remote: true})
	stubRemotePushAllowed(t, true, "private") // file:// is not GitHub; the SQL under test is real
	esc := recordCompactorEscalations(t)
	before := rd.commitCount(t)

	if got := rd.daemon(t).compactorProcessDatabase("gt", realDoltThreshold, "flatten"); got != compactorCompacted {
		t.Fatalf("outcome = %v, want compacted (divergence check must work against real Dolt)", got)
	}
	if after := rd.commitCount(t); after >= before {
		t.Errorf("history was not flattened: %d -> %d commits", before, after)
	}
	// The force-push reached the remote: after a fetch its tracking ref equals local main.
	if _, err := rd.open(t).Exec("CALL DOLT_FETCH('origin')"); err != nil {
		t.Fatal(err)
	}
	local, remote := rd.scalar(t, "SELECT DOLT_HASHOF('main')"), rd.remoteTrackingHash(t)
	if local == "" || local != remote {
		t.Errorf("remote main = %q, want the compacted local main %q", remote, local)
	}
	if n := len(esc.all()); n != 0 {
		t.Errorf("allowed remote must not escalate, got %d", n)
	}
}

// The remote has a commit local lacks: diverged, so the database is skipped
// and neither flattened nor pushed over. A silent no-op divergence check (the
// wrong column or ref name read as "no remote branch") would compact here.
func TestRealDolt_RemoteAheadIsDivergedAndSkipped(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 4, remote: true, remoteAhead: true})
	stubRemotePushAllowed(t, true, "private")
	recordCompactorEscalations(t)
	before := rd.commitCount(t)

	if got := rd.daemon(t).compactorProcessDatabase("gt", realDoltThreshold, "flatten"); got != compactorSkipped {
		t.Fatalf("outcome = %v, want skipped (remote is ahead)", got)
	}
	if after := rd.commitCount(t); after != before {
		t.Errorf("a diverged database was flattened: %d -> %d commits", before, after)
	}
}

// A remote the real guard refuses (file:// is not a private GitHub repo): the
// database is not flattened, the remote is not fetched, one escalation names
// the reason.
func TestRealDolt_RefusedRemoteIsNotFlattenedOrFetched(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 4, remote: true, remoteAhead: true})
	esc := recordCompactorEscalations(t) // the guard is NOT stubbed
	before, trackedBefore := rd.commitCount(t), rd.remoteTrackingHash(t)

	if got := rd.daemon(t).compactorProcessDatabase("gt", realDoltThreshold, "flatten"); got != compactorSkipped {
		t.Fatalf("outcome = %v, want skipped", got)
	}
	if after := rd.commitCount(t); after != before {
		t.Errorf("a database with a refused remote was flattened: %d -> %d commits", before, after)
	}
	if tracked := rd.remoteTrackingHash(t); tracked != trackedBefore {
		t.Errorf("a refused remote was fetched: tracking ref %q -> %q", trackedBefore, tracked)
	}
	calls := esc.all()
	if len(calls) != 1 || !strings.Contains(calls[0].fingerprint, "non-github-remote") {
		t.Errorf("want one escalation with reason non-github-remote, got %+v", calls)
	}
}

// A failed lookup is not "no remote". The connection pool is closed, so the
// real driver returns a real error on the remote-name query.
func TestRealDolt_FailedRemoteLookupIsARefusalNotNoRemote(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 2})
	db := rd.open(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	target := rd.daemon(t).compactorResolvePushTarget(db)
	if target.refusal != "remote-lookup-failed" {
		t.Errorf("failed lookup resolved to %+v, want refusal remote-lookup-failed", target)
	}
}

// The exact queries the compactor sends must be valid against the real schema.
func TestRealDolt_DivergenceQueriesMatchTheRealSchema(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 2, remote: true})
	stubRemotePushAllowed(t, true, "private")
	d := rd.daemon(t)
	db := rd.open(t)

	target := d.compactorResolvePushTarget(db)
	if !target.usable() || target.remote != "origin" || !strings.HasPrefix(target.url, "file://") {
		t.Fatalf("resolved %+v, want usable origin file:// remote", target)
	}
	diverged, err := d.compactorFetchAndVerify(db, "gt", target)
	if err != nil || diverged {
		t.Fatalf("in-sync remote: diverged=%v err=%v", diverged, err)
	}
}

// Remote ahead must read as diverged for the right reason: the preflight
// returns the diverged skip, not a query error that happens to skip.
func TestRealDolt_RemoteAheadReadsAsDiverged(t *testing.T) {
	rd := startRealDolt(t, realDoltFixture{commits: 2, remote: true, remoteAhead: true})
	stubRemotePushAllowed(t, true, "private")

	_, skip, err := rd.daemon(t).compactorPreflight(rd.open(t), "gt")
	if err != nil {
		t.Fatalf("preflight error: %v", err)
	}
	if skip != compactorSkipDiverged {
		t.Errorf("skip = %q, want %q", skip, compactorSkipDiverged)
	}
}

// testLogWriter routes the daemon's log lines into the test log, so a
// swallowed error (the compactor logs a failed force-push and carries on) is
// visible when a test fails.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
