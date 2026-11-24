package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const gib = uint64(1) << 30

type escalation struct {
	severity string
	message  string
}

func boolPtr(b bool) *bool { return &b }

// gibs converts fractional GiB to bytes.
func gibs(f float64) uint64 { return uint64(f * float64(1<<30)) }

// newTestDiskGuard builds a guard with a fake free-space reader and a recording
// escalator, so tests never touch the real filesystem or shell out to gt.
func newTestDiskGuard(free *uint64, freeErr *error, cfg *DiskGuardConfig) (*diskGuard, *[]escalation) {
	var got []escalation
	g := &diskGuard{
		config: cfg,
		freeBytes: func(string) (uint64, error) {
			if freeErr != nil && *freeErr != nil {
				return 0, *freeErr
			}
			return *free, nil
		},
		escalate: func(severity, message string) error {
			got = append(got, escalation{severity, message})
			return nil
		},
		logf: func(string, ...interface{}) {},
	}
	return g, &got
}

func TestDiskGuard_NoEscalationWhenPlentyFree(t *testing.T) {
	free := 200 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	g.check("/data", time.Now())
	if len(*got) != 0 {
		t.Fatalf("expected no escalation with 200 GiB free, got %+v", *got)
	}
}

func TestDiskGuard_WarnsBelowDefaultThreshold(t *testing.T) {
	free := 20 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	g.check("/data", time.Now())
	if len(*got) != 1 {
		t.Fatalf("expected 1 escalation, got %+v", *got)
	}
	if (*got)[0].severity != "HIGH" {
		t.Errorf("severity = %q, want HIGH", (*got)[0].severity)
	}
	if !strings.Contains((*got)[0].message, "/data") {
		t.Errorf("message should name the path: %q", (*got)[0].message)
	}
}

func TestDiskGuard_CriticalBelowCriticalThreshold(t *testing.T) {
	free := 5 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	g.check("/data", time.Now())
	if len(*got) != 1 || (*got)[0].severity != "CRITICAL" {
		t.Fatalf("expected one CRITICAL escalation, got %+v", *got)
	}
}

func TestDiskGuard_BoundaryIsNotLow(t *testing.T) {
	// Exactly at the threshold is not "under" it.
	free := uint64(DefaultDiskWarnFreeGB) * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	g.check("/data", time.Now())
	if len(*got) != 0 {
		t.Fatalf("exactly at warn threshold must not escalate, got %+v", *got)
	}
}

func TestDiskGuard_CustomThresholds(t *testing.T) {
	free := 50 * gib
	cfg := &DiskGuardConfig{Enabled: boolPtr(true), WarnFreeGB: 100, CriticalFreeGB: 60}
	g, got := newTestDiskGuard(&free, nil, cfg)
	g.check("/data", time.Now())
	if len(*got) != 1 || (*got)[0].severity != "CRITICAL" {
		t.Fatalf("50 GiB under custom critical=60 should be CRITICAL, got %+v", *got)
	}
}

func TestDiskGuard_InvalidThresholdsFallBackToDefaults(t *testing.T) {
	// critical >= warn is nonsensical; negative values are nonsensical.
	cases := []*DiskGuardConfig{
		{Enabled: boolPtr(true), WarnFreeGB: 10, CriticalFreeGB: 20},
		{Enabled: boolPtr(true), WarnFreeGB: -5},
		{Enabled: boolPtr(true), CriticalFreeGB: -1},
	}
	for _, cfg := range cases {
		free := 20 * gib // under default warn (30), over default critical (10)
		g, got := newTestDiskGuard(&free, nil, cfg)
		g.check("/data", time.Now())
		if len(*got) != 1 || (*got)[0].severity != "HIGH" {
			t.Errorf("cfg %+v: want one HIGH from defaults, got %+v", cfg, *got)
		}
	}
}

func TestDiskGuard_HighDoesNotRepeatWithinEpisode(t *testing.T) {
	free := 20 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	for i := 0; i < 30; i++ {
		g.check("/data", now.Add(time.Duration(i)*3*time.Hour))
	}
	if len(*got) != 1 {
		t.Fatalf("HIGH must be sent once per episode however long it lasts, got %d", len(*got))
	}
}

func TestDiskGuard_WorseningToCriticalAlertsImmediately(t *testing.T) {
	free := 20 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	g.check("/data", now)
	free = 4 * gib
	g.check("/data", now.Add(3*time.Minute))
	if len(*got) != 2 || (*got)[1].severity != "CRITICAL" {
		t.Fatalf("warn -> critical must escalate immediately, got %+v", *got)
	}
}

func TestDiskGuard_ImprovingDoesNotEscalateAndRecoveryResetsCooldown(t *testing.T) {
	free := 4 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	g.check("/data", now) // CRITICAL
	free = 20 * gib
	g.check("/data", now.Add(3*time.Minute)) // improved to warn: still in cooldown, quiet
	if len(*got) != 1 {
		t.Fatalf("critical -> warn within cooldown must stay quiet, got %+v", *got)
	}
	free = 200 * gib
	g.check("/data", now.Add(6*time.Minute)) // recovered
	free = 20 * gib
	g.check("/data", now.Add(9*time.Minute)) // fresh dip after recovery escalates at once
	if len(*got) != 2 {
		t.Fatalf("a dip after recovery must escalate immediately, got %+v", *got)
	}
}

func TestDiskGuard_StatErrorDoesNotEscalate(t *testing.T) {
	free := uint64(0)
	err := errors.New("statfs: no such file")
	g, got := newTestDiskGuard(&free, &err, nil)
	g.check("/missing", time.Now())
	if len(*got) != 0 {
		t.Fatalf("an unreadable path is not evidence of a full disk, got %+v", *got)
	}
}

func TestIsPatrolEnabled_DiskGuard(t *testing.T) {
	if !IsPatrolEnabled(nil, "disk_guard") {
		t.Error("disk_guard must default to enabled: it is a safety rail")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{DiskGuard: &DiskGuardConfig{Enabled: boolPtr(false)}}}
	if IsPatrolEnabled(off, "disk_guard") {
		t.Error("disk_guard must honor an explicit enabled:false")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{DiskGuard: &DiskGuardConfig{Enabled: boolPtr(true)}}}
	if !IsPatrolEnabled(on, "disk_guard") {
		t.Error("disk_guard must honor enabled:true")
	}
}

func TestIsPatrolEnabled_DiskGuardThresholdOnlyConfigStaysEnabled(t *testing.T) {
	for name, cfg := range map[string]*DiskGuardConfig{
		"threshold": {WarnFreeGB: 50},
		"path":      {Path: "/Volumes/data"},
		"empty":     {},
	} {
		pc := &DaemonPatrolConfig{Patrols: &PatrolsConfig{DiskGuard: cfg}}
		if !IsPatrolEnabled(pc, "disk_guard") {
			t.Errorf("%s-only config must leave the guard enabled", name)
		}
	}
}

func TestDiskGuardConfig_EnabledJSON(t *testing.T) {
	var pc DaemonPatrolConfig
	if err := json.Unmarshal([]byte(`{"patrols":{"disk_guard":{"warn_free_gb":50}}}`), &pc); err != nil {
		t.Fatal(err)
	}
	if !IsPatrolEnabled(&pc, "disk_guard") {
		t.Error("JSON without \"enabled\" must keep the guard on")
	}
	if err := json.Unmarshal([]byte(`{"patrols":{"disk_guard":{"enabled":false}}}`), &pc); err != nil {
		t.Fatal(err)
	}
	if IsPatrolEnabled(&pc, "disk_guard") {
		t.Error("explicit enabled:false must disable the guard")
	}
}

func TestDiskGuard_FailedEscalationIsRetriedNextTick(t *testing.T) {
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	attempts := 0
	g.escalate = func(severity, message string) error {
		attempts++
		if attempts == 1 {
			return errors.New("gt not found")
		}
		return nil
	}
	now := time.Now()
	g.check("/data", now)
	g.check("/data", now.Add(3*time.Minute))
	if attempts != 2 {
		t.Fatalf("a failed escalation must be retried on the next tick, attempts = %d", attempts)
	}
	g.check("/data", now.Add(6*time.Minute))
	if attempts != 2 {
		t.Fatalf("a successful escalation must start the cooldown, attempts = %d", attempts)
	}
}

func TestDiskGuard_FailedCriticalEscalationIsNotMaskedByEarlierWarn(t *testing.T) {
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	var severities []string
	fail := true
	g.escalate = func(severity, message string) error {
		severities = append(severities, severity)
		if severity == "CRITICAL" && fail {
			return errors.New("timeout")
		}
		return nil
	}
	now := time.Now()
	g.check("/data", now) // HIGH delivered
	free = 4 * gib
	g.check("/data", now.Add(3*time.Minute)) // CRITICAL fails
	fail = false
	g.check("/data", now.Add(6*time.Minute)) // CRITICAL must be retried
	want := []string{"HIGH", "CRITICAL", "CRITICAL"}
	if strings.Join(severities, ",") != strings.Join(want, ",") {
		t.Fatalf("severities = %v, want %v", severities, want)
	}
}

func TestDiskGuard_FlappingAroundWarnEscalatesOncePerCooldown(t *testing.T) {
	free := uint64(0)
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			free = gibs(29.9)
		} else {
			free = gibs(30.1)
		}
		g.check("/data", now.Add(time.Duration(i)*3*time.Minute))
	}
	if len(*got) != 1 {
		t.Fatalf("flapping 29.9/30.1 GiB must escalate once within the cooldown, got %d", len(*got))
	}
}

func TestDiskGuard_FlappingAroundCriticalEscalatesOncePerCooldown(t *testing.T) {
	free := uint64(0)
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			free = gibs(9.9)
		} else {
			free = gibs(10.1)
		}
		g.check("/data", now.Add(time.Duration(i)*time.Minute))
	}
	if len(*got) != 1 || (*got)[0].severity != "CRITICAL" {
		t.Fatalf("flapping 9.9/10.1 GiB must escalate CRITICAL once, got %+v", *got)
	}
}

func TestResolveDiskGuardPath(t *testing.T) {
	exists := func(set ...string) func(string) bool {
		return func(p string) bool {
			for _, s := range set {
				if s == p {
					return true
				}
			}
			return false
		}
	}
	tests := []struct {
		name    string
		cfg     *DiskGuardConfig
		managed string
		exists  func(string) bool
		want    string
	}{
		{"explicit wins over managed", &DiskGuardConfig{Path: "/mnt/x"}, "/mnt/dolt", exists("/mnt/x", "/mnt/dolt"), "/mnt/x"},
		{"managed data dir used", nil, "/mnt/dolt", exists("/mnt/dolt", "/town/.dolt-data"), "/mnt/dolt"},
		{"managed data dir used even when default is missing", nil, "/mnt/dolt", exists("/mnt/dolt"), "/mnt/dolt"},
		{"default used when present", nil, "", exists("/town/.dolt-data"), "/town/.dolt-data"},
		{"default falls back to town root when missing", nil, "", exists(), "/town"},
		{"missing explicit path does not switch volumes", &DiskGuardConfig{Path: "/mnt/x"}, "", exists("/town", "/town/.dolt-data"), "/mnt/x"},
		{"missing managed dir does not switch volumes", nil, "/mnt/dolt", exists("/town", "/town/.dolt-data"), "/mnt/dolt"},
	}
	for _, tc := range tests {
		if got := resolveDiskGuardPath(tc.cfg, tc.managed, "/town", tc.exists); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDiskGuard_UnreadableExplicitPathIsLoggedNotEscalatedOrSwitched(t *testing.T) {
	free := uint64(0)
	statErr := errors.New("no such file or directory")
	g, got := newTestDiskGuard(&free, &statErr, nil)
	var logged []string
	g.logf = func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	var asked []string
	inner := g.freeBytes
	g.freeBytes = func(p string) (uint64, error) { asked = append(asked, p); return inner(p) }

	path := resolveDiskGuardPath(&DiskGuardConfig{Path: "/mnt/unmounted"}, "", "/town", func(string) bool { return false })
	g.check(path, time.Now())

	if len(asked) != 1 || asked[0] != "/mnt/unmounted" {
		t.Fatalf("guard must keep watching the configured path, asked %v", asked)
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "/mnt/unmounted") {
		t.Fatalf("an unreadable configured path must be logged with its name, got %v", logged)
	}
	if len(*got) != 0 {
		t.Fatalf("unreadable path must not escalate as low disk, got %+v", *got)
	}
}

func TestDiskGuard_CriticalRepeatsEvery15MinutesWithinEpisode(t *testing.T) {
	free := 4 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	g.check("/data", now)
	g.check("/data", now.Add(14*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("no repeat before 15 minutes, got %d alerts", len(*got))
	}
	g.check("/data", now.Add(15*time.Minute))
	if len(*got) != 2 {
		t.Fatalf("CRITICAL must repeat at 15 minutes, got %d alerts", len(*got))
	}
	g.check("/data", now.Add(29*time.Minute))
	g.check("/data", now.Add(30*time.Minute))
	if len(*got) != 3 {
		t.Fatalf("CRITICAL must repeat every 15 minutes, got %d alerts", len(*got))
	}
	for _, e := range *got {
		if e.severity != "CRITICAL" {
			t.Fatalf("every repeat must be CRITICAL, got %+v", *got)
		}
	}
}

func TestDiskGuard_NewEpisodeAfterRecoveryAlertsAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		low      uint64
		severity string
	}{{"high", 20 * gib, "HIGH"}, {"critical", 4 * gib, "CRITICAL"}} {
		free := tc.low
		g, got := newTestDiskGuard(&free, nil, nil)
		now := time.Now()
		g.check("/data", now)
		free = 200 * gib
		g.check("/data", now.Add(3*time.Minute)) // recovered above threshold + margin
		free = tc.low
		g.check("/data", now.Add(6*time.Minute)) // new dip = new episode
		if len(*got) != 2 || (*got)[1].severity != tc.severity {
			t.Errorf("%s: a dip after recovery must alert again, got %+v", tc.name, *got)
		}
	}
}

func TestDiskGuard_RecoveryRequiresMarginAboveThreshold(t *testing.T) {
	free := gibs(29.9)
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	g.check("/data", now)
	free = gibs(32) // above 30 but inside the 10% margin: episode continues
	g.check("/data", now.Add(3*time.Minute))
	free = gibs(29.9)
	g.check("/data", now.Add(6*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("hovering inside the recovery margin must stay one episode, got %d alerts", len(*got))
	}
}

func TestDiskGuard_StopsAfterThreeFailedAttemptsPerAlert(t *testing.T) {
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	attempts := 0
	g.escalate = func(severity, message string) error { attempts++; return errors.New("timeout") }
	var logged []string
	g.logf = func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

	now := time.Now()
	for i := 0; i < 20; i++ {
		g.check("/data", now.Add(time.Duration(i)*3*time.Minute))
	}
	if attempts != 3 {
		t.Fatalf("a HIGH alert gets at most 3 attempts per episode, got %d", attempts)
	}
	found := false
	for _, l := range logged {
		if strings.Contains(l, "/data") && strings.Contains(l, "HIGH") && strings.Contains(strings.ToLower(l), "giving up") {
			found = true
		}
	}
	if !found {
		t.Fatalf("giving up must log an error naming the path and severity, logged: %v", logged)
	}
}

func TestDiskGuard_CriticalGivesUpThenRetriesAtNextScheduledRepeat(t *testing.T) {
	free := 4 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	attempts := 0
	g.escalate = func(severity, message string) error { attempts++; return errors.New("timeout") }
	now := time.Now()
	at := func(m int) { g.check("/data", now.Add(time.Duration(m)*time.Minute)) }

	at(0)
	at(3)
	at(6)
	at(9)
	at(12)
	if attempts != 3 {
		t.Fatalf("3 attempts then silence until the next repeat, got %d", attempts)
	}
	at(15)
	at(18)
	at(21)
	at(24)
	if attempts != 6 {
		t.Fatalf("the repeat at 15 minutes starts a fresh set of 3 attempts, got %d", attempts)
	}
}

func TestDiskGuard_AlertIsSentOnlyAfterEscalateSucceeds(t *testing.T) {
	free := 20 * gib
	g, _ := newTestDiskGuard(&free, nil, nil)
	attempts := 0
	g.escalate = func(severity, message string) error {
		attempts++
		if attempts < 3 {
			return errors.New("timeout")
		}
		return nil
	}
	now := time.Now()
	for i := 0; i < 8; i++ {
		g.check("/data", now.Add(time.Duration(i)*3*time.Minute))
	}
	if attempts != 3 {
		t.Fatalf("two failures then a success, then quiet: attempts = %d, want 3", attempts)
	}
}

func TestDiskGuard_EpisodesArePerPath(t *testing.T) {
	free := 20 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	g.check("/a", now)
	g.check("/b", now.Add(3*time.Minute))
	if len(*got) != 2 {
		t.Fatalf("a different path is a different episode, got %d alerts", len(*got))
	}
}

func TestDiskGuardEscalateArgs_NoFingerprint(t *testing.T) {
	for _, a := range diskGuardEscalateArgs("HIGH", "low") {
		if a == "--fingerprint" {
			t.Fatal("escalation must not use fingerprint dedup: a suppressed retry can hide a lost alert")
		}
	}
}
