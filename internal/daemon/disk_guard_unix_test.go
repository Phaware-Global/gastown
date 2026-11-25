//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// writeFakeGt writes an executable shell script standing in for gt. Tests
// never run the real gt, so they can never file a live escalation or mail.
func writeFakeGt(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-gt")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// The fake gt writes its state and then hangs past the timeout, like a gt
// escalate that wrote the bead and was stuck before sending mail. The alert is
// not delivered: it must be retried on the next heartbeat, and the retries
// must stop after 3 attempts.
func TestDiskGuardEscalation_HangAfterWriteIsRetriedThreeTimesThenStops(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	gt := writeFakeGt(t, dir, "echo wrote >> "+calls+"\nexec sleep 300\n")

	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	g.escalate = func(severity, message string) error {
		return runDiskGuardEscalation(gt, dir, []string{"escalate"}, 300*time.Millisecond, func(string, ...interface{}) {})
	}
	now := time.Now()
	g.check("/data", now)
	if n := countLines(t, calls); n != 1 {
		t.Fatalf("first attempt: gt called %d times", n)
	}
	g.check("/data", now.Add(3*time.Minute))
	if n := countLines(t, calls); n != 2 {
		t.Fatalf("a timed-out alert is not sent; it must be retried next heartbeat, gt called %d times", n)
	}
	for i := 2; i < 8; i++ {
		g.check("/data", now.Add(time.Duration(i)*3*time.Minute))
	}
	if n := countLines(t, calls); n != 3 {
		t.Fatalf("retries must stop after 3 attempts, gt called %d times", n)
	}
}

// zombieOrGone reports whether pid no longer runs: it does not exist, or it is
// a zombie waiting for a reaper (a killed process whose parent died stays a
// zombie in a container without an init that reaps).
func zombieOrGone(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

const helperEnv = "GT_DISKGUARD_TEST_HELPER"

// orphanEnv makes the helper's late child lose its parent as soon as it starts.
const orphanEnv = "GT_DISKGUARD_TEST_ORPHAN"

func init() { helperProcessHook = runDiskGuardHelper }

// runDiskGuardHelper is the body of the fake gt's helper process. The fake gt
// re-execs the test binary into it (the usual Go helper-process pattern, run
// from TestMain before any setup) so the process topology does not depend on
// shell job control, which differs between shells and platforms. As gt does
// with bd, it starts its child with Setpgid, so the child leads a process
// group of its own. It returns immediately in an ordinary test run.
func runDiskGuardHelper() {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		return
	}
	child := exec.Command("sleep", "300")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	childPgid, _ := syscall.Getpgid(child.Process.Pid)
	record := map[string]int{
		"pid":        child.Process.Pid,
		"pgid":       childPgid,
		"parentpgid": syscall.Getpgrp(),
	}
	for name, v := range record {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(v)), 0o644); err != nil {
			os.Exit(4)
		}
	}
	go spawnLateChild(dir)
	_ = child.Wait()
	os.Exit(0)
}

// spawnLateChild starts a second child, in its own process group, once the
// "spawn" file appears, and records its pid in "latepid".
func spawnLateChild(dir string) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "spawn")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if os.Getenv(orphanEnv) != "" {
		spawnOrphan(dir)
		return
	}
	late := exec.Command("sleep", "300")
	late.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := late.Start(); err != nil {
		os.Exit(5)
	}
	tmp := filepath.Join(dir, "latepid.tmp")
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(late.Process.Pid)), 0o644); err != nil {
		os.Exit(6)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "latepid")); err != nil {
		os.Exit(7)
	}
}

// spawnOrphan starts a shell, in its own process group, that backgrounds a
// sleep and exits. "latepid" appears only after the shell is reaped, so by then
// the sleep has been reparented: neither ancestry nor gt's process group leads
// to it, only its session.
func spawnOrphan(dir string) {
	tmp := filepath.Join(dir, "latepid.tmp")
	sh := exec.Command("sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $! > \"$1\"", "sh", tmp)
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sh.Run(); err != nil {
		os.Exit(5)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "latepid")); err != nil {
		os.Exit(7)
	}
}

// writeFakePs puts a ps on PATH that behaves like the real one, except that
// once the "armed" file exists, its next call takes the real listing, has the
// helper spawn its late child, and only then returns that stale listing.
func writeFakePs(t *testing.T, dir string) {
	t.Helper()
	realPs, err := exec.LookPath("ps")
	if err != nil {
		t.Skipf("no ps: %v", err)
	}
	q := func(name string) string { return "'" + filepath.Join(dir, name) + "'" }
	script := "#!/bin/sh\n" +
		"if [ ! -e " + q("armed") + " ] || [ -e " + q("first") + " ]; then exec " + realPs + " \"$@\"; fi\n" +
		": > " + q("first") + "\n" +
		"out=$(" + realPs + " \"$@\")\n" +
		": > " + q("spawn") + "\n" +
		"while [ ! -e " + q("latepid") + " ]; do sleep 0.01; done\n" +
		"printf '%s\\n' \"$out\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// cancelHelperEscalation runs an escalation against the helper gt, cancels it
// once the helper has recorded its topology, and waits for it to return. It
// creates the "armed" file just before canceling.
func cancelHelperEscalation(t *testing.T, dir string) {
	t.Helper()
	t.Setenv(helperEnv, dir)
	gt := writeHelperGt(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runDiskGuardEscalationCtx(ctx, gt, dir, []string{"escalate"}, 20*time.Second, func(string, ...interface{}) {})
	}()
	ready := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "parentpgid")); err == nil {
			break
		}
		if time.Now().After(ready) {
			t.Fatal("helper never recorded its topology")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Earlier escalations list processes once more, a grace period after they
	// were canceled. Let those calls happen first, or one of them would use up
	// the fake ps's single stale listing.
	time.Sleep(diskGuardEscalateGrace + time.Second)
	if err := os.WriteFile(filepath.Join(dir, "armed"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled escalation must report an error")
	}
}

// requireKilled fails unless the process recorded in dir/name stops running.
func requireKilled(t *testing.T, dir, name string) {
	t.Helper()
	pid, err := strconv.Atoi(readTrimmed(t, filepath.Join(dir, name)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if zombieOrGone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: process %d survived the escalation timeout", name, pid)
}

// writeHelperGt writes a fake gt that re-execs this test binary into the
// helper process above (selected by helperEnv).
func writeHelperGt(t *testing.T, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return writeFakeGt(t, dir, "exec '"+exe+"'\n")
}

// bd is started by gt in its own process group, so a group kill of gt alone
// misses it. The fake gt starts a grandchild the same way, and a timeout must
// still take it down.
func TestDiskGuardEscalation_TimeoutKillsGrandchildInItsOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(helperEnv, dir)
	gt := writeHelperGt(t, dir)

	// Cancel only after the helper has recorded its topology; the test binary
	// takes about a second to start, so a short timeout would race it. A
	// cancelled context takes the same kill path as a deadline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runDiskGuardEscalationCtx(ctx, gt, dir, []string{"escalate"}, 20*time.Second, func(string, ...interface{}) {})
	}()
	ready := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "parentpgid")); err == nil {
			break
		}
		if time.Now().After(ready) {
			cancel()
			t.Fatal("helper never recorded its topology")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let the other record files land
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled escalation must report an error")
	}

	read := func(name string) int {
		n, perr := strconv.Atoi(readTrimmed(t, filepath.Join(dir, name)))
		if perr != nil {
			t.Fatal(perr)
		}
		return n
	}
	pid, pgid, parentPgid := read("pid"), read("pgid"), read("parentpgid")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if pgid != pid || pgid == parentPgid {
		t.Fatalf("test topology is wrong: grandchild pid %d pgid %d must lead its own group, distinct from gt's group %d", pid, pgid, parentPgid)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if zombieOrGone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d (own process group) survived the escalation timeout", pid)
}

func TestDiskGuardEscalation_BdTimeoutIsBelowOuterTimeout(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env")
	gt := writeFakeGt(t, dir, "echo \"$GT_BD_TIMEOUT_SEC\" > "+out+"\n")
	if err := runDiskGuardEscalation(gt, dir, []string{"escalate"}, 20*time.Second, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(readTrimmed(t, out))
	if err != nil || n <= 0 || n >= 20 {
		t.Fatalf("GT_BD_TIMEOUT_SEC = %q, want a positive integer below the 20s outer timeout", readTrimmed(t, out))
	}
}

// gt escalate prints the created line after it has written the bead and sent
// mail, and can still exit non-zero (a configured notification has no contact).
// The alert was delivered: retrying would file it again.
func TestDiskGuardEscalation_CreatedThenNonZeroExitIsFiledOnce(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	gt := writeFakeGt(t, dir, "echo filed >> "+calls+"\n"+
		"echo '🟠 Escalation created: hq-fake'\n"+
		"echo 'Error: 1 configured notification(s) were skipped' >&2\n"+
		"exit 1\n")

	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	g.logf = logf
	g.escalate = func(severity, message string) error {
		return runDiskGuardEscalation(gt, dir, []string{"escalate"}, 20*time.Second, logf)
	}
	now := time.Now()
	for i := 0; i < 8; i++ {
		g.check("/data", now.Add(time.Duration(i)*3*time.Minute))
	}
	if n := countLines(t, calls); n != 1 {
		t.Fatalf("a filed escalation must not be filed again, gt called %d times", n)
	}
	all := strings.Join(logged, "\n")
	if strings.Contains(all, "giving up") {
		t.Fatalf("a delivered alert must not log a give-up error:\n%s", all)
	}
	if !strings.Contains(all, "notification") || !strings.Contains(all, "were skipped") {
		t.Fatalf("the notification failure must be logged:\n%s", all)
	}
}

func TestDiskGuardEscalation_FailureBeforeWriteIsAnError(t *testing.T) {
	dir := t.TempDir()
	gt := writeFakeGt(t, dir, "echo 'Error: creating escalation bead: refused' >&2\nexit 1\n")
	if err := runDiskGuardEscalation(gt, dir, []string{"escalate"}, 20*time.Second, func(string, ...interface{}) {}); err == nil {
		t.Fatal("a failure before the escalation was written must be retried")
	}
}

// A timeout is retried even after the created line: gt did not finish, and a
// bounded duplicate is better than a lost alert.
func TestDiskGuardEscalation_TimeoutAfterCreatedLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	gt := writeFakeGt(t, dir, "echo '🟠 Escalation created: hq-fake'\nexec sleep 300\n")
	if err := runDiskGuardEscalation(gt, dir, []string{"escalate"}, 500*time.Millisecond, func(string, ...interface{}) {}); err == nil {
		t.Fatal("a timed-out escalation must report an error")
	}
}

// Without a process listing, descendants outside gt's process group cannot be
// found and may be orphaned, so the failure has to show up in the log.
func TestDiskGuardEscalation_ProcessListFailureIsLogged(t *testing.T) {
	dir := t.TempDir()
	gt := writeFakeGt(t, dir, "exec /bin/sleep 300\n")
	t.Setenv("PATH", dir) // no ps here

	var mu sync.Mutex
	var logged []string
	logf := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	if err := runDiskGuardEscalation(gt, dir, []string{"escalate"}, 300*time.Millisecond, logf); err == nil {
		t.Fatal("a timed-out escalation must report an error")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range logged {
		if strings.Contains(l, "cannot list processes") {
			return
		}
	}
	t.Fatalf("a failing ps must be logged, got %v", logged)
}

// A child gt starts after the process tree was snapshotted must still be taken
// down. The fake ps makes the window deterministic.
func TestDiskGuardEscalation_TimeoutKillsChildSpawnedAfterSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFakePs(t, dir)
	cancelHelperEscalation(t, dir)
	requireKilled(t, dir, "latepid")
}

// Such a child may also have lost its parent by the second listing, as a bd
// does once gt dies. Only its session ID still ties it to gt.
func TestDiskGuardEscalation_TimeoutKillsReparentedLateChildBySession(t *testing.T) {
	dir := t.TempDir()
	writeFakePs(t, dir)
	t.Setenv(orphanEnv, "1")
	cancelHelperEscalation(t, dir)
	requireKilled(t, dir, "latepid")
}

// A pid that exited between the two listings may name an unrelated process by
// the second one. Neither it nor its children are gt's to signal.
func TestLateProcesses(t *testing.T) {
	// No real process leads a session with this ID, so only ancestry matches.
	const root = 1 << 30
	known := []procInfo{{pid: root + 1, ppid: root, started: "then"}}
	child := procInfo{pid: root + 2, ppid: root + 1, started: "later"}
	for _, tc := range []struct {
		name   string
		parent string // start time of the known pid at the second listing
		want   []procInfo
	}{
		{"same process", "then", []procInfo{child}},
		{"reused pid", "later", nil},
	} {
		procs := map[int]procInfo{
			root + 1:  {pid: root + 1, ppid: 1, started: tc.parent},
			child.pid: child,
		}
		if got := lateProcesses(procs, root, known); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
