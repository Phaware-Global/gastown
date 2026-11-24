//go:build !windows

package daemon

import (
	"io"
	"log"
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

func fakeGtDaemon(dir, gt string) *Daemon {
	return &Daemon{
		config: &Config{TownRoot: dir},
		logger: log.New(io.Discard, "", 0),
		gtPath: gt,
	}
}

// A fake gt that follows the real CLI contract: with --fingerprint it files
// once and answers later calls for the same fingerprint with "duplicate
// suppressed" (exit 0) while the first is open. Without one it files every time.
// Filing exits non-zero, like gt escalate does when a notification contact is
// missing after the bead and mail were already written.
const fakeGtWritesThenFails = `
fp=""
while [ $# -gt 0 ]; do
  case "$1" in --fingerprint) fp="$2" ;; esac
  shift
done
if [ -n "$fp" ] && grep -qx -- "$fp" "$STATE" 2>/dev/null; then
  echo "Duplicate escalation suppressed"
  exit 0
fi
[ -n "$fp" ] && echo "$fp" >> "$STATE"
echo filed >> "$FILED"
echo "escalation created but notification skipped" >&2
exit 1
`

func TestDiskGuardEscalation_DeliveredButNonZeroIsNotRefiled(t *testing.T) {
	dir := t.TempDir()
	state, filed := filepath.Join(dir, "state"), filepath.Join(dir, "filed")
	gt := writeFakeGt(t, dir, "STATE="+state+"\nFILED="+filed+"\n"+fakeGtWritesThenFails)

	d := fakeGtDaemon(dir, gt)
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	g.escalate = func(severity, message string) error { return d.escalateDiskFree(severity, "/data", message) }

	now := time.Now()
	for i := 0; i < 5; i++ {
		// The message carries the free-bytes figure, which changes every tick.
		free = 20*gib - uint64(i)*(1<<20)
		g.check("/data", now.Add(time.Duration(i)*3*time.Minute))
	}
	if n := countLines(t, filed); n != 1 {
		t.Fatalf("a delivered escalation must be filed once across heartbeats, filed %d times", n)
	}
}

func TestDiskGuardEscalation_FailureBeforeAnyWriteIsRetried(t *testing.T) {
	dir := t.TempDir()
	calls, filed := filepath.Join(dir, "calls"), filepath.Join(dir, "filed")
	gt := writeFakeGt(t, dir, `
echo call >> `+calls+`
if [ "$(wc -l < `+calls+`)" -lt 2 ]; then echo "bd: write timeout" >&2; exit 1; fi
echo filed >> `+filed+`
exit 0
`)
	d := fakeGtDaemon(dir, gt)
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	g.escalate = func(severity, message string) error { return d.escalateDiskFree(severity, "/data", message) }

	now := time.Now()
	g.check("/data", now)
	g.check("/data", now.Add(3*time.Minute))
	g.check("/data", now.Add(6*time.Minute))
	if n := countLines(t, calls); n != 2 {
		t.Fatalf("expected a retry after a failure with no write, then quiet; gt called %d times", n)
	}
	if n := countLines(t, filed); n != 1 {
		t.Fatalf("expected one filed escalation, got %d", n)
	}
}

func TestDiskGuardEscalateArgs_FingerprintIsStable(t *testing.T) {
	fp := func(args []string) string {
		for i, a := range args {
			if a == "--fingerprint" && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	a := fp(diskGuardEscalateArgs("HIGH", "/data", "Disk space low: 20.0 GB free"))
	b := fp(diskGuardEscalateArgs("HIGH", "/data", "Disk space low: 19.9 GB free"))
	if a == "" {
		t.Fatal("escalation must carry a --fingerprint")
	}
	if a != b {
		t.Errorf("fingerprint must not depend on the free-bytes text: %q vs %q", a, b)
	}
	if a == fp(diskGuardEscalateArgs("CRITICAL", "/data", "x")) {
		t.Error("fingerprint must differ by severity")
	}
	if a == fp(diskGuardEscalateArgs("HIGH", "/other", "x")) {
		t.Error("fingerprint must differ by path")
	}
	if want := "disk_guard:/data:HIGH"; a != want {
		t.Errorf("fingerprint = %q, want %q", a, want)
	}
}

func TestDiskGuardEscalation_TimeoutKillsGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	gt := writeFakeGt(t, dir, "sleep 300 &\necho $! > "+pidFile+"\nwait\n")

	err := runDiskGuardEscalation(gt, dir, []string{"escalate"}, 500*time.Millisecond, func(string, ...interface{}) {})
	if err == nil {
		t.Fatal("a timed-out escalation must report an error")
	}

	data, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("fake gt never recorded its grandchild: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		t.Fatal(perr)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d survived the escalation timeout", pid)
}
