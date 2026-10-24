package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// INVARIANT under test: a database with a configured Dolt remote is never
// flattened in a cycle that cannot push to it. If the remote exists but the
// push guard refuses it for ANY reason, the database is skipped for the cycle
// (no fetch, no flatten, no push) and escalated once per db and reason. A
// database with no remote compacts locally.

type escalationCall struct{ fingerprint, title, reason string }

type escalationRecorder struct {
	mu    sync.Mutex
	calls []escalationCall
}

func (r *escalationRecorder) all() []escalationCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]escalationCall(nil), r.calls...)
}

func recordCompactorEscalations(t *testing.T) *escalationRecorder {
	t.Helper()
	rec := &escalationRecorder{}
	orig := compactorEscalate
	t.Cleanup(func() { compactorEscalate = orig })
	compactorEscalate = func(_ *Daemon, fingerprint, title, reason string) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.calls = append(rec.calls, escalationCall{fingerprint, title, reason})
	}
	return rec
}

const policyTestURL = "git+https://github.com/example-owner/repo"

func flattened(conn *fakeDoltConn) bool {
	return len(conn.execsContaining("DOLT_RESET")) > 0 || len(conn.execsContaining("DOLT_COMMIT")) > 0
}

// A refusal — transient (the lookup failed, gh is missing) or definitive
// (public, DoltHub, unreadable URL) — means the database is not flattened:
// flattening without being able to push would leave the remote's old HEAD
// missing from local history, and the database would read as diverged on the
// next cycle that can reach the remote.
func TestCompactorProcessDatabase_RefusedRemoteIsNeverFlattened(t *testing.T) {
	for _, reason := range []string{
		"visibility-lookup-failed", "gh-unavailable", // transient
		"public", "internal", "non-github-remote", "dolthub-unsupported", // definitive
	} {
		t.Run(reason, func(t *testing.T) {
			conn := &fakeDoltConn{remoteName: "origin", remoteURL: policyTestURL, remoteHead: "oldhead", headInLog: true, commits: 5000}
			useFakeDoltForCompactor(t, conn)
			stubRemotePushAllowed(t, false, reason)
			esc := recordCompactorEscalations(t)

			got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten")

			if got != compactorSkipped {
				t.Errorf("outcome = %v, want skipped", got)
			}
			if flattened(conn) {
				t.Errorf("database was flattened although its remote cannot be pushed: %v", conn.execs)
			}
			if f := conn.fetches(); len(f) != 0 {
				t.Errorf("refused remote was fetched: %v", f)
			}
			if p := conn.pushes(); len(p) != 0 {
				t.Errorf("refused remote was pushed: %v", p)
			}
			calls := esc.all()
			if len(calls) != 1 {
				t.Fatalf("want exactly one escalation, got %d: %+v", len(calls), calls)
			}
			if !strings.Contains(calls[0].fingerprint, "gt") || !strings.Contains(calls[0].fingerprint, reason) {
				t.Errorf("fingerprint %q should be per db and reason (%q)", calls[0].fingerprint, reason)
			}
		})
	}
}

func TestCompactorProcessDatabase_UnresolvableURLIsNeverFlattened(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: "", commits: 5000}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, true, "private") // must not be reached
	esc := recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorSkipped {
		t.Errorf("outcome = %v, want skipped", got)
	}
	if flattened(conn) || len(conn.pushes()) != 0 || len(conn.fetches()) != 0 {
		t.Errorf("remote with unresolvable URL was touched: %v", conn.execs)
	}
	if n := len(esc.all()); n != 1 {
		t.Errorf("want one escalation, got %d", n)
	}
}

// The escalation key is stable across cycles for the same db and reason (an
// open escalation then suppresses repeats), and distinct per db and per reason.
func TestCompactorProcessDatabase_EscalationFingerprintIsPerDbAndReason(t *testing.T) {
	esc := recordCompactorEscalations(t)
	run := func(db, reason string) {
		conn := &fakeDoltConn{db: db, remoteName: "origin", remoteURL: policyTestURL, commits: 5000}
		useFakeDoltForCompactor(t, conn)
		stubRemotePushAllowed(t, false, reason)
		compactorTestDaemon().compactorProcessDatabase(db, 2000, "flatten")
	}
	run("gt", "public")
	run("gt", "public") // next cycle, same condition
	run("hq", "public")
	run("gt", "gh-unavailable")

	calls := esc.all()
	if len(calls) != 4 {
		t.Fatalf("want 4 escalation calls, got %d", len(calls))
	}
	if calls[0].fingerprint != calls[1].fingerprint {
		t.Errorf("same db and reason must reuse the fingerprint: %q vs %q", calls[0].fingerprint, calls[1].fingerprint)
	}
	if calls[2].fingerprint == calls[0].fingerprint {
		t.Error("a different db must have a different fingerprint")
	}
	if calls[3].fingerprint == calls[0].fingerprint {
		t.Error("a different reason must have a different fingerprint")
	}
}

// No remote: nothing to push, so nothing to protect; compacts locally.
func TestCompactorProcessDatabase_NoRemoteCompactsLocally(t *testing.T) {
	conn := &fakeDoltConn{commits: 5000}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, false, "public") // must not matter without a remote
	esc := recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorCompacted {
		t.Fatalf("outcome = %v, want compacted", got)
	}
	if !flattened(conn) {
		t.Error("database with no remote was not compacted")
	}
	if len(conn.pushes()) != 0 || len(conn.fetches()) != 0 {
		t.Errorf("no remote, yet fetch/push ran: %v", conn.execs)
	}
	if n := len(esc.all()); n != 0 {
		t.Errorf("no remote must not escalate, got %d", n)
	}
}

// Guard passes: fetched, compacted, force-pushed, no escalation.
func TestCompactorProcessDatabase_AllowedRemoteCompactsAndPushes(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: policyTestURL, remoteHead: "samehead", headInLog: true, commits: 5000}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, true, "private")
	esc := recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorCompacted {
		t.Fatalf("outcome = %v, want compacted", got)
	}
	if !flattened(conn) {
		t.Error("allowed remote: database was not compacted")
	}
	if len(conn.fetches()) != 1 {
		t.Errorf("want one fetch, got %v", conn.fetches())
	}
	if p := conn.pushes(); len(p) != 1 || !strings.Contains(p[0], "--force") {
		t.Errorf("want one force-push, got %v", p)
	}
	if n := len(esc.all()); n != 0 {
		t.Errorf("allowed remote must not escalate, got %d", n)
	}
}

// A genuinely diverged (allowed) remote is still skipped, as before.
func TestCompactorProcessDatabase_DivergedRemoteIsSkipped(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: policyTestURL, remoteHead: "remoteonly", headInLog: false, commits: 5000}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, true, "private")
	recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorSkipped {
		t.Errorf("outcome = %v, want skipped", got)
	}
	if flattened(conn) || len(conn.pushes()) != 0 {
		t.Errorf("diverged remote: flattened or pushed: %v", conn.execs)
	}
}

func TestCompactorProcessDatabase_BelowThresholdIsSkipped(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: policyTestURL, commits: 10}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, false, "public")
	esc := recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorSkipped {
		t.Errorf("outcome = %v, want skipped", got)
	}
	if n := len(esc.all()); n != 0 {
		t.Errorf("a database below the threshold must not escalate a refusal, got %d", n)
	}
}

// Every read in the push-target path must tell "failed" from "empty". Only an
// empty answer to a successful query means "no remote"; a failed read is an
// unknown destination and, by the invariant, the database is skipped and
// escalated rather than flattened with no push.
func TestCompactorProcessDatabase_FailedRemoteNameLookupIsNotNoRemote(t *testing.T) {
	conn := &fakeDoltConn{nameErr: errors.New("connection reset by peer"), remoteName: "origin", remoteURL: policyTestURL, commits: 5000}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, true, "private") // must not be reached
	esc := recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorSkipped {
		t.Errorf("outcome = %v, want skipped", got)
	}
	if flattened(conn) || len(conn.fetches()) != 0 || len(conn.pushes()) != 0 {
		t.Errorf("a failed remote lookup was read as 'no remote' and the database was touched: %v", conn.execs)
	}
	calls := esc.all()
	if len(calls) != 1 {
		t.Fatalf("want exactly one escalation, got %d: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[0].fingerprint, "gt") || !strings.Contains(calls[0].fingerprint, "remote-lookup-failed") {
		t.Errorf("fingerprint %q should be per db and reason remote-lookup-failed", calls[0].fingerprint)
	}
	// The remote's name is unknown (the read that would give it failed), so the
	// message must say the lookup failed, not that a remote "" was refused.
	for _, text := range []string{calls[0].title, calls[0].reason} {
		if strings.Contains(text, `remote ""`) || strings.Contains(text, "refused") {
			t.Errorf("escalation text presents a failed lookup as a refusal of remote \"\": %q", text)
		}
		if !strings.Contains(text, "gt") {
			t.Errorf("escalation text should name the database: %q", text)
		}
	}
	if !strings.Contains(calls[0].reason, "could not read") {
		t.Errorf("reason should say the remote configuration could not be read: %q", calls[0].reason)
	}
}

// A failed URL read is a refusal too, not an absent URL that slips through.
func TestCompactorProcessDatabase_FailedRemoteURLLookupIsNeverFlattened(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", urlErr: errors.New("i/o timeout"), commits: 5000}
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, true, "private") // must not be reached
	esc := recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorSkipped {
		t.Errorf("outcome = %v, want skipped", got)
	}
	if flattened(conn) || len(conn.fetches()) != 0 || len(conn.pushes()) != 0 {
		t.Errorf("failed URL lookup: database was touched: %v", conn.execs)
	}
	if n := len(esc.all()); n != 1 {
		t.Errorf("want one escalation, got %d", n)
	}
}

// An empty result of a SUCCESSFUL lookup is the only "no remote".
func TestCompactorResolvePushTarget_EmptyNameIsNoRemote(t *testing.T) {
	conn := &fakeDoltConn{}
	db := openFakeDolt(t, conn)
	target := compactorTestDaemon().compactorResolvePushTarget(db)
	if target.remote != "" || target.refusal != "" {
		t.Errorf("empty name lookup should be no remote, got %+v", target)
	}
}

// The divergence read after the fetch: a failed read means the check could not
// be made, so the database is skipped — not flattened and force-pushed over
// commits that might exist only on the remote. A remote with no main branch
// yet is a successful empty read and proceeds.
func TestCompactorProcessDatabase_FailedDivergenceReadsAreNotAbsence(t *testing.T) {
	cases := map[string]*fakeDoltConn{
		"remote branch lookup": {remoteName: "origin", remoteURL: policyTestURL, commits: 5000, branchErr: errors.New("lost connection")},
		"ancestor lookup":      {remoteName: "origin", remoteURL: policyTestURL, commits: 5000, remoteHead: "somehead", logErr: errors.New("lost connection")},
	}
	for name, conn := range cases {
		t.Run(name, func(t *testing.T) {
			useFakeDoltForCompactor(t, conn)
			stubRemotePushAllowed(t, true, "private")
			recordCompactorEscalations(t)

			if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorSkipped {
				t.Errorf("outcome = %v, want skipped", got)
			}
			if flattened(conn) || len(conn.pushes()) != 0 {
				t.Errorf("an unverifiable remote was flattened or pushed: %v", conn.execs)
			}
		})
	}
}

func TestCompactorProcessDatabase_RemoteWithNoMainBranchYetCompactsAndPushes(t *testing.T) {
	conn := &fakeDoltConn{remoteName: "origin", remoteURL: policyTestURL, commits: 5000} // remoteHead "" = no rows
	useFakeDoltForCompactor(t, conn)
	stubRemotePushAllowed(t, true, "private")
	recordCompactorEscalations(t)

	if got := compactorTestDaemon().compactorProcessDatabase("gt", 2000, "flatten"); got != compactorCompacted {
		t.Fatalf("outcome = %v, want compacted", got)
	}
	if !flattened(conn) || len(conn.pushes()) != 1 {
		t.Errorf("want compact + one push, got %v", conn.execs)
	}
}
