package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeDoltConn is a database/sql driver that answers the dolt_remotes lookups
// compactorForcePush makes and records every statement it is asked to
// execute, so a test can tell whether a DOLT_PUSH was attempted.
type fakeDoltConn struct {
	db         string // database name the compactor queries ("" = "gt")
	remoteName string // "" = no remote configured
	remoteURL  string // "" = url lookup returns no row
	remoteHead string // "" = the remote has no main branch ref
	headInLog  bool   // whether remoteHead is in the local dolt_log
	commits    int64  // dolt_log length, for compactorProcessDatabase

	// Injected read failures: an error is not an empty result.
	nameErr   error // the dolt_remotes name lookup fails
	urlErr    error // the dolt_remotes url lookup fails
	branchErr error // the dolt_remote_branches lookup fails
	logErr    error // the ancestor lookup in dolt_log fails
	mu        sync.Mutex
	execs     []string
}

func (c *fakeDoltConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakeDoltConn) Close() error                        { return nil }
func (c *fakeDoltConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

// The fake answers ONLY the exact query text, bind arguments and result
// columns the compactor sends for its database (default "gt") — no substring
// matching, no invented schema. Anything else is an error, so a query that
// drifts from what real Dolt accepts fails loudly here and in the real-Dolt
// tests (compactor_realdolt_integration_test.go), instead of being matched by
// a fragment and answered with columns Dolt does not have.
const (
	qRemoteName   = "SELECT name FROM dolt_remotes LIMIT 1"
	qRemoteURL    = "SELECT url FROM dolt_remotes WHERE name = ?"
	qRemoteBranch = "SELECT hash FROM dolt_remote_branches WHERE name = ?"
	qAncestor     = "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?"
)

func (c *fakeDoltConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	arg := func(i int) string {
		if i < len(args) {
			return fmt.Sprint(args[i].Value)
		}
		return ""
	}
	one := func(col string, v driver.Value) driver.Rows {
		return &fakeRows{cols: []string{col}, rows: [][]driver.Value{{v}}}
	}
	none := func(col string) driver.Rows { return &fakeRows{cols: []string{col}} }

	db := c.db
	if db == "" {
		db = "gt"
	}
	switch q {
	case qRemoteName:
		if c.nameErr != nil {
			return nil, c.nameErr
		}
		if c.remoteName == "" {
			return none("name"), nil
		}
		return one("name", c.remoteName), nil
	case qRemoteURL:
		if c.urlErr != nil {
			return nil, c.urlErr
		}
		if c.remoteURL == "" || arg(0) != c.remoteName {
			return none("url"), nil
		}
		return one("url", c.remoteURL), nil
	case qRemoteBranch:
		if c.branchErr != nil {
			return nil, c.branchErr
		}
		// Real Dolt names the ref "remotes/<remote>/main", not "<remote>/main".
		if c.remoteHead == "" || arg(0) != "remotes/"+c.remoteName+"/main" {
			return none("hash"), nil
		}
		return one("hash", c.remoteHead), nil
	case qAncestor:
		if c.logErr != nil {
			return nil, c.logErr
		}
		n := int64(0)
		if c.headInLog && arg(0) == c.remoteHead {
			n = 1
		}
		return one("COUNT(*)", n), nil
	case "SELECT COUNT(*) FROM `" + db + "`.dolt_log":
		return one("COUNT(*)", c.commits), nil
	case "SELECT table_name FROM information_schema.tables WHERE table_schema = '" + db + "' AND table_name NOT LIKE 'dolt_%'":
		return none("table_name"), nil
	case "SELECT commit_hash FROM `" + db + "`.dolt_log ORDER BY date ASC LIMIT 1":
		return one("commit_hash", "rootrootroot"), nil
	}
	return nil, fmt.Errorf("fakeDoltConn: unexpected query %q (args %v) — the fake answers only exact query text", q, args)
}

func (c *fakeDoltConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := q
	for _, a := range args {
		rec += fmt.Sprintf(" %v", a.Value)
	}
	c.execs = append(c.execs, rec)
	return driver.ResultNoRows, nil
}

func (c *fakeDoltConn) execsContaining(sub string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.execs {
		if strings.Contains(e, sub) {
			out = append(out, e)
		}
	}
	return out
}

func (c *fakeDoltConn) pushes() []string  { return c.execsContaining("DOLT_PUSH") }
func (c *fakeDoltConn) fetches() []string { return c.execsContaining("DOLT_FETCH") }

// resolveAndPush is the push step of one compaction cycle for a database
// whose push target is decided once, up front.
func resolveAndPush(d *Daemon, db *sql.DB, dbName string) error {
	return d.compactorForcePushDB(db, dbName, d.compactorResolvePushTarget(db))
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

type fakeDoltDriver struct{ conn *fakeDoltConn }

func (d fakeDoltDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

var fakeDriverSeq atomic.Int64

func openFakeDolt(t *testing.T, conn *fakeDoltConn) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("fakedolt-%d", fakeDriverSeq.Add(1))
	sql.Register(name, fakeDoltDriver{conn: conn})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// useFakeDoltForCompactor makes compactorOpenDB hand out connections to conn.
func useFakeDoltForCompactor(t *testing.T, conn *fakeDoltConn) {
	t.Helper()
	name := fmt.Sprintf("fakedolt-%d", fakeDriverSeq.Add(1))
	sql.Register(name, fakeDoltDriver{conn: conn})
	orig := compactorOpenDriver
	t.Cleanup(func() { compactorOpenDriver = orig })
	compactorOpenDriver = name
}

func compactorTestDaemon() *Daemon {
	return &Daemon{logger: log.New(io.Discard, "", 0)}
}

func TestCompactorForcePush_RefusesNonPrivateRemote(t *testing.T) {
	for _, reason := range []string{"public", "internal", "visibility-lookup-failed", "gh-unavailable", "non-github-remote", "dolthub-unsupported"} {
		t.Run(reason, func(t *testing.T) {
			conn := &fakeDoltConn{remoteName: "origin", remoteURL: "git+https://github.com/example-owner/repo"}
			db := openFakeDolt(t, conn)
			stubRemotePushAllowed(t, false, reason)

			err := resolveAndPush(compactorTestDaemon(), db, "gt")
			if err == nil {
				t.Fatalf("force-push to a remote with verdict %q returned nil", reason)
			}
			if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), reason) {
				t.Errorf("error should say REFUSED and name %q, got: %v", reason, err)
			}
			if p := conn.pushes(); len(p) != 0 {
				t.Errorf("DOLT_PUSH ran despite the refusal: %v", p)
			}
		})
	}
}

// A remote whose URL cannot be read is an unknown destination: fail closed.
func TestCompactorForcePush_RefusesWhenRemoteURLUnknown(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: ""}
	db := openFakeDolt(t, conn)
	stubRemotePushAllowed(t, true, "private") // must not be reached

	if err := resolveAndPush(compactorTestDaemon(), db, "gt"); err == nil {
		t.Fatal("force-push with an unresolvable remote URL returned nil")
	}
	if p := conn.pushes(); len(p) != 0 {
		t.Errorf("DOLT_PUSH ran with no resolvable URL: %v", p)
	}
}

func TestCompactorForcePush_AllowsPrivateRemote(t *testing.T) {
	const url = "git+https://github.com/example-owner/repo"
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: url}
	db := openFakeDolt(t, conn)

	var asked []string
	orig := remotePushAllowed
	t.Cleanup(func() { remotePushAllowed = orig })
	remotePushAllowed = func(_ context.Context, u string) (bool, string) {
		asked = append(asked, u)
		return true, "private"
	}

	if err := resolveAndPush(compactorTestDaemon(), db, "gt"); err != nil {
		t.Fatalf("private remote refused: %v", err)
	}
	if len(asked) != 1 || asked[0] != url {
		t.Errorf("verdict asked about %v, want the remote's URL %q", asked, url)
	}
	if p := conn.pushes(); len(p) != 1 || !strings.Contains(p[0], "--force") || !strings.Contains(p[0], "origin") {
		t.Errorf("want one force-push to origin, got %v", p)
	}
}

// No remote configured stays the quiet no-op it always was.
func TestCompactorForcePush_NoRemoteIsNoop(t *testing.T) {
	conn := &fakeDoltConn{}
	db := openFakeDolt(t, conn)
	stubRemotePushAllowed(t, false, "public") // must not matter

	if err := resolveAndPush(compactorTestDaemon(), db, "gt"); err != nil {
		t.Fatalf("no remote should be skipped silently, got: %v", err)
	}
	if p := conn.pushes(); len(p) != 0 {
		t.Errorf("pushed with no remote: %v", p)
	}
}

// The push target (remote, URL, guard verdict) is decided once per database,
// before anything touches the remote. A refused remote is no push target at
// all: it is never fetched, so it can never read as diverged after a
// compaction rewrote the history, and it is never pushed.
func TestCompactorPreflight_RefusedRemoteIsNeverFetched(t *testing.T) {
	for _, reason := range []string{"public", "visibility-lookup-failed", "gh-unavailable", "dolthub-unsupported"} {
		t.Run(reason, func(t *testing.T) {
			// The remote's old HEAD is not in the (rewritten) local log: if it
			// were fetched and compared, this database would read as diverged.
			conn := &fakeDoltConn{remoteName: "origin", remoteURL: "git+https://github.com/example-owner/repo", remoteHead: "oldhead", headInLog: false}
			db := openFakeDolt(t, conn)
			stubRemotePushAllowed(t, false, reason)
			d := compactorTestDaemon()

			for cycle := 1; cycle <= 2; cycle++ {
				target, skip, err := d.compactorPreflight(db, "gt")
				if err != nil {
					t.Fatalf("cycle %d: preflight error: %v", cycle, err)
				}
				if skip != compactorSkipPushRefused {
					t.Errorf("cycle %d: skip = %q, want %q: a database whose remote cannot be pushed is never flattened", cycle, skip, compactorSkipPushRefused)
				}
				if target.usable() {
					t.Errorf("cycle %d: refused remote is still a usable push target", cycle)
				}
				if target.refusal != reason {
					t.Errorf("cycle %d: refusal = %q, want %q", cycle, target.refusal, reason)
				}
			}
			if f := conn.fetches(); len(f) != 0 {
				t.Errorf("refused remote was fetched: %v", f)
			}
		})
	}
}

func TestCompactorPreflight_UnresolvableURLIsNeverFetched(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: "", remoteHead: "oldhead"}
	db := openFakeDolt(t, conn)
	stubRemotePushAllowed(t, true, "private") // must not be reached

	target, skip, err := compactorTestDaemon().compactorPreflight(db, "gt")
	if err != nil || skip != compactorSkipPushRefused || target.usable() {
		t.Fatalf("got usable=%v skip=%q err=%v, want unusable, skipped (push refused), no error", target.usable(), skip, err)
	}
	if f := conn.fetches(); len(f) != 0 {
		t.Errorf("remote with unresolvable URL was fetched: %v", f)
	}
}

// Positive control: an allowed remote is fetched and divergence is still
// detected, so the refused-remote test above cannot pass by never looking.
func TestCompactorPreflight_AllowedRemoteIsFetchedAndDivergenceDetected(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: "git+https://github.com/example-owner/repo", remoteHead: "remoteonly", headInLog: false}
	db := openFakeDolt(t, conn)
	stubRemotePushAllowed(t, true, "private")

	target, skip, err := compactorTestDaemon().compactorPreflight(db, "gt")
	if err != nil {
		t.Fatal(err)
	}
	if !target.usable() || target.remote != "origin" {
		t.Errorf("private remote should be the push target, got %+v", target)
	}
	if skip != compactorSkipDiverged {
		t.Errorf("remote HEAD missing from local history must skip as diverged, got skip=%q", skip)
	}
	if f := conn.fetches(); len(f) != 1 {
		t.Errorf("want one fetch of the allowed remote, got %v", f)
	}
}

func TestCompactorPreflight_AllowedRemoteInSyncIsPushable(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: "git+https://github.com/example-owner/repo", remoteHead: "samehead", headInLog: true}
	db := openFakeDolt(t, conn)
	stubRemotePushAllowed(t, true, "private")
	d := compactorTestDaemon()

	target, skip, err := d.compactorPreflight(db, "gt")
	if err != nil || skip != "" || !target.usable() {
		t.Fatalf("usable=%v skip=%q err=%v", target.usable(), skip, err)
	}
	if err := d.compactorForcePushDB(db, "gt", target); err != nil {
		t.Fatal(err)
	}
	if p := conn.pushes(); len(p) != 1 {
		t.Errorf("want one force-push, got %v", p)
	}
}

func TestCompactorPreflight_NoRemoteHasNoPushTarget(t *testing.T) {
	conn := &fakeDoltConn{}
	db := openFakeDolt(t, conn)
	stubRemotePushAllowed(t, false, "public") // must not matter

	d := compactorTestDaemon()
	target, skip, err := d.compactorPreflight(db, "gt")
	if err != nil || skip != "" || target.usable() || target.refusal != "" {
		t.Fatalf("no remote: usable=%v refusal=%q skip=%q err=%v", target.usable(), target.refusal, skip, err)
	}
	if err := d.compactorForcePushDB(db, "gt", target); err != nil {
		t.Errorf("no remote should be a quiet no-op, got: %v", err)
	}
}

// Even handed a refused target directly, the push step refuses.
func TestCompactorForcePushDB_RefusesARefusedTarget(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin"}
	db := openFakeDolt(t, conn)
	target := compactorPushTarget{remote: "origin", url: "git+https://github.com/example-owner/repo", refusal: "public"}

	if err := compactorTestDaemon().compactorForcePushDB(db, "gt", target); err == nil {
		t.Fatal("pushed to a refused target")
	}
	if p := conn.pushes(); len(p) != 0 {
		t.Errorf("DOLT_PUSH ran for a refused target: %v", p)
	}
}
