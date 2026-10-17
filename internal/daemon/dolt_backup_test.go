package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDoltOffsiteEnabled(t *testing.T) {
	if !doltOffsiteEnabled(nil) {
		t.Error("nil config should default to offsite enabled")
	}
	if !doltOffsiteEnabled(&DoltBackupConfig{Enabled: true}) {
		t.Error("unset offsite should default to enabled")
	}
	off := false
	if doltOffsiteEnabled(&DoltBackupConfig{Enabled: true, Offsite: &off}) {
		t.Error("offsite=false should disable the offsite sync")
	}
	on := true
	if !doltOffsiteEnabled(&DoltBackupConfig{Enabled: true, Offsite: &on}) {
		t.Error("offsite=true should enable the offsite sync")
	}
}

// closedPort returns a loopback port with nothing listening on it.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// townDoltEndpoint points the unmanaged-server default address at a loopback
// port, listening or not, and returns the listener (nil when not listening).
func townDoltEndpoint(t *testing.T, listening bool) net.Listener {
	t.Helper()
	t.Setenv("GT_DOLT_HOST", "127.0.0.1")
	if !listening {
		t.Setenv("GT_DOLT_PORT", strconv.Itoa(closedPort(t)))
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	return ln
}

func TestDoltServerAcceptingConnections(t *testing.T) {
	var logs []string
	logf := func(format string, v ...interface{}) { logs = append(logs, fmt.Sprintf(format, v...)) }
	townRoot := t.TempDir()

	// Not daemon-managed (nil or disabled): the town's server is checked.
	disabled := NewDoltServerManager(t.TempDir(), &DoltServerConfig{Enabled: false, Host: "127.0.0.1", Port: 1}, logf)
	unmanaged := map[string]*Daemon{
		"nil manager":      {config: &Config{TownRoot: townRoot}},
		"disabled manager": {config: &Config{TownRoot: townRoot}, doltServer: disabled},
	}

	ln := townDoltEndpoint(t, true)
	for name, d := range unmanaged {
		if !d.doltServerAcceptingConnections() {
			t.Errorf("%s: town server listening: backups should run", name)
		}
	}
	ln.Close()
	for name, d := range unmanaged {
		if d.doltServerAcceptingConnections() {
			t.Errorf("%s: town server down: backups must be skipped", name)
		}
	}

	// Daemon-managed: the managed server's own port decides.
	mln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := mln.Addr().(*net.TCPAddr).Port
	cfg := &DoltServerConfig{Enabled: true, Host: "127.0.0.1", Port: port}
	d := &Daemon{config: &Config{TownRoot: townRoot}, doltServer: NewDoltServerManager(t.TempDir(), cfg, logf)}
	if !d.doltServerAcceptingConnections() {
		t.Error("listening server: backups should run")
	}

	mln.Close()
	if d.doltServerAcceptingConnections() {
		t.Error("server down: backups must be skipped")
	}
}

// fakeDolt writes a `dolt` stub that records each invocation's directory and
// then sleeps or exits per mode, and puts it first on PATH.
func fakeDolt(t *testing.T, body string) (callLog string) {
	t.Helper()
	bin := t.TempDir()
	callLog = filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\npwd >> " + callLog + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "dolt"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return callLog
}

func callCount(t *testing.T, callLog string) int {
	t.Helper()
	data, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func newBackupTestDaemon(t *testing.T, ctx context.Context) (*Daemon, string) {
	t.Helper()
	dataDir := t.TempDir()
	for _, db := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(dataDir, db), 0755); err != nil {
			t.Fatal(err)
		}
	}
	townDoltEndpoint(t, true)
	return &Daemon{ctx: ctx, config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}, dataDir
}

func TestSyncBackupTimeoutIsNotRetried(t *testing.T) {
	calls := fakeDolt(t, "sleep 30")
	d, dataDir := newBackupTestDaemon(t, context.Background())

	origTimeout, origDelay := doltBackupTimeout, doltBackupRetryDelay
	doltBackupTimeout, doltBackupRetryDelay = time.Second, time.Millisecond
	t.Cleanup(func() { doltBackupTimeout, doltBackupRetryDelay = origTimeout, origDelay })

	err := d.syncBackup(dataDir, "a", "a-backup")
	if !errors.Is(err, errDoltBackupTimeout) {
		t.Fatalf("err = %v, want errDoltBackupTimeout", err)
	}
	if n := callCount(t, calls); n != 1 {
		t.Errorf("dolt invoked %d times, want 1 (a timeout must not be retried)", n)
	}
}

func TestSyncBackupRetriesPlainFailure(t *testing.T) {
	calls := fakeDolt(t, "exit 1")
	d, dataDir := newBackupTestDaemon(t, context.Background())

	orig := doltBackupRetryDelay
	doltBackupRetryDelay = time.Millisecond
	t.Cleanup(func() { doltBackupRetryDelay = orig })

	err := d.syncBackup(dataDir, "a", "a-backup")
	if err == nil || errors.Is(err, errDoltBackupTimeout) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
	if n := callCount(t, calls); n != 2 {
		t.Errorf("dolt invoked %d times, want 2 (one retry)", n)
	}
}

func serverManager(t *testing.T, listening bool) *DoltServerManager {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if listening {
		t.Cleanup(func() { ln.Close() })
	} else {
		ln.Close()
	}
	cfg := &DoltServerConfig{Enabled: true, Host: "127.0.0.1", Port: port}
	return NewDoltServerManager(t.TempDir(), cfg, func(string, ...interface{}) {})
}

func TestSyncBackupCycleSkipsWhenServerDown(t *testing.T) {
	calls := fakeDolt(t, "exit 0")
	d, dataDir := newBackupTestDaemon(t, context.Background())
	d.doltServer = serverManager(t, false)

	synced, failures := d.syncBackupCycle(dataDir, []string{"a", "b"})
	if synced != 0 || !reflect.DeepEqual(failures, []string{"a", "b"}) {
		t.Errorf("synced=%d failures=%v, want 0 and [a b]", synced, failures)
	}
	if n := callCount(t, calls); n != 0 {
		t.Errorf("dolt invoked %d times with the server down; it would take the DB lock directly", n)
	}
}

func TestSyncBackupCycleSyncsWhenServerUp(t *testing.T) {
	calls := fakeDolt(t, "exit 0")
	d, dataDir := newBackupTestDaemon(t, context.Background())
	d.doltServer = serverManager(t, true)

	synced, failures := d.syncBackupCycle(dataDir, []string{"a", "b"})
	if synced != 2 || len(failures) != 0 {
		t.Errorf("synced=%d failures=%v, want 2 and none", synced, failures)
	}
	if n := callCount(t, calls); n != 2 {
		t.Errorf("dolt invoked %d times, want 2", n)
	}
}

func TestSyncBackupCycleAbortsAfterTimeout(t *testing.T) {
	calls := fakeDolt(t, "sleep 30")
	d, dataDir := newBackupTestDaemon(t, context.Background())

	origTimeout, origDelay := doltBackupTimeout, doltBackupRetryDelay
	doltBackupTimeout, doltBackupRetryDelay = time.Second, time.Millisecond
	t.Cleanup(func() { doltBackupTimeout, doltBackupRetryDelay = origTimeout, origDelay })

	synced, failures := d.syncBackupCycle(dataDir, []string{"a", "b"})
	if synced != 0 || !reflect.DeepEqual(failures, []string{"a", "b"}) {
		t.Errorf("synced=%d failures=%v, want 0 and [a b]", synced, failures)
	}
	if n := callCount(t, calls); n != 1 {
		t.Errorf("dolt invoked %d times, want 1: the second database must not be attempted after a timeout", n)
	}
}

func TestSyncBackupCycleSkipsWhenUnmanagedServerDown(t *testing.T) {
	calls := fakeDolt(t, "exit 0")
	d, dataDir := newBackupTestDaemon(t, context.Background())
	townDoltEndpoint(t, false)

	synced, failures := d.syncBackupCycle(dataDir, []string{"a", "b"})
	if synced != 0 || !reflect.DeepEqual(failures, []string{"a", "b"}) {
		t.Errorf("synced=%d failures=%v, want 0 and [a b]", synced, failures)
	}
	if n := callCount(t, calls); n != 0 {
		t.Errorf("dolt invoked %d times with the town server down; it would take the DB lock directly", n)
	}
}

func TestSyncBackupDoesNotRetryAfterServerStops(t *testing.T) {
	d, dataDir := newBackupTestDaemon(t, context.Background())
	ln := townDoltEndpoint(t, true)
	// The first attempt fails and takes the server down with it.
	calls := fakeDolt(t, "exit 1")
	script := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "dolt")
	body := "#!/bin/sh\npwd >> " + calls + "\ntouch " + calls + ".down\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			if _, err := os.Stat(calls + ".down"); err == nil {
				ln.Close()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	orig := doltBackupRetryDelay
	doltBackupRetryDelay = 200 * time.Millisecond
	t.Cleanup(func() { doltBackupRetryDelay = orig })

	err := d.syncBackup(dataDir, "a", "a-backup")
	<-stopped
	if err == nil || errors.Is(err, errDoltBackupTimeout) {
		t.Fatalf("err = %v, want the first attempt's plain failure", err)
	}
	if n := callCount(t, calls); n != 1 {
		t.Errorf("dolt invoked %d times, want 1: the retry must not run against a down server", n)
	}
}
