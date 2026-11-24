//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func gone(pid int) bool { return syscall.Kill(pid, 0) != nil }

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

// bd is started by gt in its own process group, so the fake gt starts its
// grandchild the same way (job control gives it a group of its own). A timeout
// must still take it down.
func TestDiskGuardEscalation_TimeoutKillsGrandchildInItsOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile, pgidFile := filepath.Join(dir, "pid"), filepath.Join(dir, "pgid")
	gt := writeFakeGt(t, dir, "set -m\nsleep 300 &\npid=$!\necho $pid > "+pidFile+"\nps -o pgid= -p $pid > "+pgidFile+"\nwait\n")

	err := runDiskGuardEscalation(gt, dir, []string{"escalate"}, 500*time.Millisecond, func(string, ...interface{}) {})
	if err == nil {
		t.Fatal("a timed-out escalation must report an error")
	}

	pid, perr := strconv.Atoi(readTrimmed(t, pidFile))
	if perr != nil {
		t.Fatal(perr)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if got := readTrimmed(t, pgidFile); got != strconv.Itoa(pid) {
		t.Fatalf("test topology is wrong: grandchild pgid %q is not its own pid %d", got, pid)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gone(pid) {
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
