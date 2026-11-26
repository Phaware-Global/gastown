package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/wisp"
)

// writeFakeTestBD creates a shell script in dir named "bd" that outputs a
// polecat agent bead JSON. The descState parameter controls what appears in
// the description text (parsed by ParseAgentFields), while
// dbState controls the agent_state database column. updatedAt controls the
// bead's updated_at timestamp for time-bound testing.
func writeFakeTestBD(t *testing.T, dir, descState, dbState, hookBead, updatedAt string) string {
	t.Helper()
	desc := "agent_state: " + descState
	// JSON matches the structure that getAgentBeadInfo expects from bd show --json
	bdJSON := fmt.Sprintf(`[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"%s","hook_bead":"%s","agent_state":"%s","updated_at":"%s"}]`,
		desc, hookBead, dbState, updatedAt)
	// Return agent bead JSON for "show", empty array for "list" (so
	// hasAssignedOpenWork doesn't false-positive on the agent bead).
	script := "#!/bin/sh\nif [ \"$1\" = \"list\" ]; then echo '[]'; exit 0; fi\necho '" + bdJSON + "'\n"
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return path
}

// writeFakeBDWithHookBead creates a shell script in dir named "bd" that returns
// different JSON based on the bead ID: an agent bead carrying a legacy hook_bead,
// and the bead that slot points to. "list" finds no work.
func writeFakeBDWithHookBead(t *testing.T, dir, agentState, hookBeadID, hookBeadStatus, updatedAt string) string {
	t.Helper()
	agentJSON := fmt.Sprintf(`[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: %s","hook_bead":"%s","agent_state":"%s","updated_at":"%s"}]`,
		agentState, hookBeadID, agentState, updatedAt)
	hookJSON := fmt.Sprintf(`[{"id":"%s","status":"%s"}]`, hookBeadID, hookBeadStatus)
	script := fmt.Sprintf("#!/bin/sh\n"+
		"if [ \"$1\" = \"list\" ]; then echo '[]'; exit 0; fi\n"+
		"case \"$2\" in\n"+
		"  gt-myr-polecat-mycat) echo '%s';;\n"+
		"  %s) echo '%s';;\n"+
		"  *) echo '[]'; exit 1;;\n"+
		"esac\n", agentJSON, hookBeadID, hookJSON)
	bdPath := filepath.Join(dir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return bdPath
}

// newCrashFixture is a rig with one polecat, mycat, whose work exists only on the
// work bead.
func newCrashFixture(t *testing.T, sessionAlive bool, list, show []map[string]string) *stuckWorkFixture {
	t.Helper()
	f := newStuckWorkFixture(t, sessionAlive, list, show)
	dir := filepath.Join(f.d.config.TownRoot, "myr", "polecats", "mycat")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating polecat dir: %v", err)
	}
	return f
}

// setAgent replaces the polecat's agent bead.
func (f *stuckWorkFixture) setAgent(t *testing.T, descState, dbState string, age time.Duration) {
	t.Helper()
	data, err := json.Marshal([]map[string]any{{
		"id": "gt-myr-polecat-mycat", "issue_type": "task", "labels": []string{"gt:agent"},
		"description": "agent_state: " + descState, "agent_state": dbState, "updated_at": ago(age),
	}})
	if err != nil {
		t.Fatalf("marshaling agent bead: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.binDir, "agent.json"), data, 0644); err != nil {
		t.Fatalf("writing agent bead: %v", err)
	}
}

func (f *stuckWorkFixture) setScript(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.binDir, name), []byte("#!/bin/sh\n"+body), 0755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
}

func TestCheckRigPolecatHealth_ReportsCrashFromWorkBead(t *testing.T) {
	for _, status := range []string{"hooked", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			bead := workBead(status, "myr/polecats/mycat", time.Hour)
			f := newCrashFixture(t, false, bead, bead)

			f.d.checkRigPolecatHealth("myr")

			if !strings.Contains(f.logs.String(), "CRASH DETECTED") {
				t.Errorf("expected CRASH DETECTED, got log: %s", f.logs)
			}
			got := f.mail(t)
			for _, want := range []string{"mail send myr/witness", "CRASHED_POLECAT", "gt-work1"} {
				if !strings.Contains(got, want) {
					t.Errorf("expected mail containing %q, got %q", want, got)
				}
			}
		})
	}
}

func TestCheckRigPolecatHealth_Quiet(t *testing.T) {
	held := workBead("hooked", "myr/polecats/mycat", time.Hour)
	cases := map[string]struct {
		alive, parked bool
		agentState    string
		list, show    []map[string]string
	}{
		"live session":   {alive: true, list: held, show: held},
		"no hooked work": {},
		"closed since":   {list: held, show: workBead("closed", "myr/polecats/mycat", time.Hour)},
		"reassigned":     {list: held, show: workBead("hooked", "myr/polecats/other", time.Hour)},
		"done":           {agentState: "done", list: held, show: held},
		"nuked":          {agentState: "nuked", list: held, show: held},
		"idle":           {agentState: "idle", list: held, show: held},
		"stuck":          {agentState: "stuck", list: held, show: held},
		"spawning":       {agentState: "spawning", list: held, show: held},
		"parked rig":     {parked: true, list: held, show: held},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCrashFixture(t, tc.alive, tc.list, tc.show)
			if tc.agentState != "" {
				f.setAgent(t, tc.agentState, tc.agentState, time.Minute)
			}
			if tc.parked {
				if err := wisp.NewConfig(f.d.config.TownRoot, "myr").Set("status", "parked"); err != nil {
					t.Fatalf("parking rig: %v", err)
				}
			}

			f.d.checkRigPolecatHealth("myr")

			if got := f.mail(t); got != "" {
				t.Errorf("expected no mail, got %q", got)
			}
			if strings.Contains(f.logs.String(), "CRASH DETECTED") {
				t.Errorf("expected no crash, got log: %s", f.logs)
			}
		})
	}
}

// A gt sling that died mid-spawn leaves agent_state=spawning behind; the guard
// against double-spawning (issue #1752) is therefore time-bound.
func TestCheckRigPolecatHealth_SpawningGuardExpires(t *testing.T) {
	bead := workBead("hooked", "myr/polecats/mycat", time.Hour)
	f := newCrashFixture(t, false, bead, bead)
	f.setAgent(t, "spawning", "spawning", 10*time.Minute)

	f.d.checkRigPolecatHealth("myr")

	for _, want := range []string{"Spawning guard expired", "CRASH DETECTED"} {
		if !strings.Contains(f.logs.String(), want) {
			t.Errorf("expected %q in log, got: %s", want, f.logs)
		}
	}
}

// The description's agent_state is the active contract; the DB column is a
// legacy fallback.
func TestCheckRigPolecatHealth_DescriptionStateOverridesLegacyDBColumn(t *testing.T) {
	bead := workBead("hooked", "myr/polecats/mycat", time.Hour)
	f := newCrashFixture(t, false, bead, bead)
	f.setAgent(t, "spawning", "working", time.Minute)

	f.d.checkRigPolecatHealth("myr")

	if got := f.mail(t); got != "" {
		t.Errorf("expected no mail, got %q", got)
	}
	if !strings.Contains(f.logs.String(), "agent_state=spawning") {
		t.Errorf("expected spawning skip, got log: %s", f.logs)
	}
}

func TestCheckRigPolecatHealth_ReportsEachCrashOnce(t *testing.T) {
	bead := workBead("hooked", "myr/polecats/mycat", time.Hour)
	f := newCrashFixture(t, false, bead, bead)

	f.d.checkRigPolecatHealth("myr")
	f.d.checkRigPolecatHealth("myr")

	if got := strings.Count(f.mail(t), "mail send"); got != 1 {
		t.Errorf("expected 1 mail across two heartbeats, got %d", got)
	}
	if got := len(f.d.recentDeaths); got != 1 {
		t.Errorf("expected 1 recorded death, got %d", got)
	}

	// The session recovers and dies again.
	dead, err := os.ReadFile(filepath.Join(f.binDir, "tmux"))
	if err != nil {
		t.Fatalf("reading fake tmux: %v", err)
	}
	f.setScript(t, "tmux", "exit 0\n")
	f.d.checkRigPolecatHealth("myr")
	f.setScript(t, "tmux", strings.TrimPrefix(string(dead), "#!/bin/sh\n"))
	f.d.checkRigPolecatHealth("myr")

	if got := strings.Count(f.mail(t), "mail send"); got != 2 {
		t.Errorf("expected a second mail for the second crash, got %d", got)
	}
}

func TestCheckRigPolecatHealth_RetriesFailedMailWithoutRecordingAnotherDeath(t *testing.T) {
	bead := workBead("hooked", "myr/polecats/mycat", time.Hour)
	f := newCrashFixture(t, false, bead, bead)
	f.setScript(t, "gt", `echo "$*" >> `+f.mailLog+"\nexit 1\n")

	f.d.checkRigPolecatHealth("myr")
	f.d.checkRigPolecatHealth("myr")

	if got := strings.Count(f.mail(t), "mail send"); got != 2 {
		t.Errorf("expected the failed mail to be retried, got %d attempts", got)
	}
	if got := len(f.d.recentDeaths); got != 1 {
		t.Errorf("expected 1 recorded death, got %d", got)
	}
}

// writeFakeTmuxWithAgent creates a shell script that simulates a live tmux session
// with an agent process running. has-session succeeds, display-message returns the
// given paneCommand (e.g., "claude" or "codex") so IsAgentRunning returns true.
func writeFakeTmuxWithAgent(t *testing.T, dir, paneCommand string) {
	t.Helper()
	// Use $* glob matching (not $1) because tmux.run() prepends -u (and
	// optionally -L <socket>) before the subcommand.
	script := fmt.Sprintf("#!/bin/sh\n"+
		"case \"$*\" in\n"+
		"  *has-session*) exit 0;;\n"+
		"  *display-message*) echo '%s';;\n"+
		"  *kill-session*) exit 0;;\n"+
		"  *) exit 1;;\n"+
		"esac\n", paneCommand)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0755); err != nil {
		t.Fatalf("writing fake tmux: %v", err)
	}
}

// writeFakeTmuxIdleSession creates a shell script that simulates a live tmux session
// with NO agent process running (idle shell). has-session succeeds, display-message
// returns "bash" so IsAgentRunning returns false.
func writeFakeTmuxIdleSession(t *testing.T, dir string) {
	t.Helper()
	writeFakeTmuxWithAgent(t, dir, "bash")
}

// writeFakeBDLookupFail creates a "bd" script that fails on "show" (simulating a
// bead infrastructure error) but returns configurable output for "list" queries.
// When hasWork is true, "list" returns an open work bead assigned to "myr/polecats/mycat".
func writeFakeBDLookupFail(t *testing.T, dir string, hasWork bool) string {
	t.Helper()
	listOut := `[]`
	if hasWork {
		listOut = `[{"id":"wh-test-1","status":"open","assignee":"myr/polecats/mycat"}]`
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"list\" ]; then echo '" + listOut + "'; exit 0; fi\n" +
		"# show fails — simulate bead infrastructure degradation\n" +
		"exit 1\n"
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return path
}

// TestReapIdlePolecat_SkipsWhenBeadLookupFailsButHasWork verifies that reapIdlePolecat
// does NOT kill a polecat when the agent bead lookup fails but hasAssignedOpenWork
// confirms the polecat has an open work bead assigned. This is the regression test
// for the working-bead-lookup-failed kill bug (GH#3342 followup).
func TestReapIdlePolecat_SkipsWhenBeadLookupFailsButHasWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux and bd")
	}
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	writeFakeTmuxIdleSession(t, binDir)
	bdPath := writeFakeBDLookupFail(t, binDir, true /* hasWork */)

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmuxWithSocket(""),
		bdPath: bdPath,
	}

	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0755)
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-60 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 0)

	if strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Errorf("must NOT reap polecat with open assigned work when agent bead lookup fails, got: %q", logBuf.String())
	}
}

// TestReapIdlePolecat_ReapsWhenBeadLookupFailsAndNoWork verifies that reapIdlePolecat
// DOES kill a polecat when the agent bead lookup fails, no work is assigned, and the
// agent process is not running. Ensures the hasAssignedOpenWork guard doesn't over-protect.
func TestReapIdlePolecat_ReapsWhenBeadLookupFailsAndNoWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux and bd")
	}
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	writeFakeTmuxIdleSession(t, binDir)
	bdPath := writeFakeBDLookupFail(t, binDir, false /* no work */)

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmuxWithSocket(""),
		bdPath: bdPath,
	}

	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0755)
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-45 * time.Minute), // 3x the 15m timeout
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 0)

	if !strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Errorf("expected idle polecat with no work and failed bead lookup to be reaped, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "working-bead-lookup-failed") {
		t.Errorf("expected working-bead-lookup-failed reason, got: %q", logBuf.String())
	}
}

// TestReapIdlePolecat_SkipsActiveAgent verifies that reapIdlePolecat does NOT kill
// a polecat whose hook_bead is missing but whose agent process is still running.
// This is the regression test for GH#3342: a failed gt sling rollback can clear
// the hook while the agent is actively working, causing the daemon to incorrectly
// reap the session.
func TestReapIdlePolecat_SkipsActiveAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux and bd")
	}
	// Register "myr" prefix so session name resolves to "myr-mycat"
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	// Fake tmux: session alive, agent (codex) running in pane
	writeFakeTmuxWithAgent(t, binDir, "codex")
	// Fake bd: agent bead exists but hook_bead is empty (cleared by failed sling)
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "", recentTime)

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmux(),
		bdPath: bdPath,
	}

	// Write a stale heartbeat (working state, 20 minutes old) so the reaper considers it
	polecat.TouchSessionHeartbeatWithState(townRoot, "myr-mycat", polecat.HeartbeatWorking, "", "")
	// Backdate the heartbeat to make it stale
	hbPath := filepath.Join(townRoot, "heartbeats", "myr-mycat.json")
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-20 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 0)

	got := logBuf.String()
	if strings.Contains(got, "Reaping idle polecat") {
		t.Errorf("must NOT reap polecat with active agent process (GH#3342), got: %q", got)
	}
}

// TestReapIdlePolecat_ReapsIdleNoHook verifies that reapIdlePolecat DOES kill
// a polecat whose hook_bead is missing AND whose agent process is NOT running
// (idle shell). This ensures the GH#3342 fix doesn't prevent legitimate reaping.
func TestReapIdlePolecat_ReapsIdleNoHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux and bd")
	}
	// Register "myr" prefix so session name resolves to "myr-mycat"
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	// Fake tmux: session alive, but only a shell running (no agent)
	writeFakeTmuxIdleSession(t, binDir)
	// Fake bd: agent bead exists but hook_bead is empty
	recentTime := time.Now().UTC().Format(time.RFC3339)
	bdPath := writeFakeTestBD(t, binDir, "working", "working", "", recentTime)

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	townRoot := t.TempDir()
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmuxWithSocket(""),
		bdPath: bdPath,
	}

	// Write a stale heartbeat (working state, 20 minutes old) so the reaper considers it
	polecat.TouchSessionHeartbeatWithState(townRoot, "myr-mycat", polecat.HeartbeatWorking, "", "")
	// Backdate the heartbeat to make it stale
	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	staleHB := polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-20 * time.Minute),
		State:     polecat.HeartbeatWorking,
	}
	data, _ := json.Marshal(staleHB)
	_ = os.WriteFile(hbPath, data, 0644)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 0)

	got := logBuf.String()
	if !strings.Contains(got, "Reaping idle polecat") {
		t.Errorf("expected idle polecat with no agent to be reaped, got: %q", got)
	}
	if !strings.Contains(got, "working-no-hook") {
		t.Errorf("expected working-no-hook reason, got: %q", got)
	}
}

// A legacy hook_bead left on an agent bead says nothing about current work.
func TestReapIdlePolecat_IgnoresLegacyHookBead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for tmux and bd")
	}
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	defer session.SetDefaultRegistry(old)

	binDir := t.TempDir()
	writeFakeTmuxIdleSession(t, binDir)
	bdPath := writeFakeBDWithHookBead(t, binDir, "working", "gt-xyz", "open", ago(time.Hour))
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	townRoot := t.TempDir()
	t.Chdir(townRoot)
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		tmux:   tmux.NewTmuxWithSocket(""),
		bdPath: bdPath,
	}

	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	if err := os.MkdirAll(filepath.Dir(hbPath), 0755); err != nil {
		t.Fatalf("creating heartbeat dir: %v", err)
	}
	data, err := json.Marshal(polecat.SessionHeartbeat{
		Timestamp: time.Now().UTC().Add(-20 * time.Minute),
		State:     polecat.HeartbeatWorking,
	})
	if err != nil {
		t.Fatalf("marshaling heartbeat: %v", err)
	}
	if err := os.WriteFile(hbPath, data, 0644); err != nil {
		t.Fatalf("writing heartbeat: %v", err)
	}

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute, 0)

	if !strings.Contains(logBuf.String(), "working-no-hook") {
		t.Errorf("expected polecat with no assigned work to be reaped, got: %q", logBuf.String())
	}
}
