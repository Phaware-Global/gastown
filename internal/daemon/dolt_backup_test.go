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

func TestDoltServerAcceptingConnections(t *testing.T) {
	var logs []string
	logf := func(format string, v ...interface{}) { logs = append(logs, fmt.Sprintf(format, v...)) }

	if !(&Daemon{}).doltServerAcceptingConnections() {
		t.Error("no managed server: backups should not be gated")
	}

	disabled := NewDoltServerManager(t.TempDir(), &DoltServerConfig{Enabled: false, Host: "127.0.0.1", Port: 1}, logf)
	if !(&Daemon{doltServer: disabled}).doltServerAcceptingConnections() {
		t.Error("disabled server management: backups should not be gated")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := &DoltServerConfig{Enabled: true, Host: "127.0.0.1", Port: port}
	d := &Daemon{doltServer: NewDoltServerManager(t.TempDir(), cfg, logf)}
	if !d.doltServerAcceptingConnections() {
		t.Error("listening server: backups should run")
	}

	ln.Close()
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
	return &Daemon{ctx: ctx, logger: log.New(io.Discard, "", 0)}, dataDir
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
