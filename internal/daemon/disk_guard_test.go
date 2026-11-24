package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const gib = uint64(1) << 30

type escalation struct {
	severity string
	message  string
}

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
		escalate: func(severity, message string) {
			got = append(got, escalation{severity, message})
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
	cfg := &DiskGuardConfig{Enabled: true, WarnFreeGB: 100, CriticalFreeGB: 60}
	g, got := newTestDiskGuard(&free, nil, cfg)
	g.check("/data", time.Now())
	if len(*got) != 1 || (*got)[0].severity != "CRITICAL" {
		t.Fatalf("50 GiB under custom critical=60 should be CRITICAL, got %+v", *got)
	}
}

func TestDiskGuard_InvalidThresholdsFallBackToDefaults(t *testing.T) {
	// critical >= warn is nonsensical; negative values are nonsensical.
	cases := []*DiskGuardConfig{
		{Enabled: true, WarnFreeGB: 10, CriticalFreeGB: 20},
		{Enabled: true, WarnFreeGB: -5},
		{Enabled: true, CriticalFreeGB: -1},
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

func TestDiskGuard_CooldownSuppressesRepeatEscalation(t *testing.T) {
	free := 20 * gib
	g, got := newTestDiskGuard(&free, nil, nil)
	now := time.Now()
	g.check("/data", now)
	g.check("/data", now.Add(3*time.Minute))
	g.check("/data", now.Add(30*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("expected escalation once within cooldown, got %d: %+v", len(*got), *got)
	}
	g.check("/data", now.Add(DiskGuardWarnCooldown+time.Minute))
	if len(*got) != 2 {
		t.Fatalf("expected re-escalation after cooldown, got %d", len(*got))
	}
}

func TestDiskGuard_WorseningBypassesCooldown(t *testing.T) {
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

func TestDiskGuardPath_DefaultsToDoltDataDir(t *testing.T) {
	got := diskGuardPath(nil, "/town")
	if got != "/town/.dolt-data" {
		t.Errorf("default path = %q, want /town/.dolt-data", got)
	}
	got = diskGuardPath(&DiskGuardConfig{Path: "/mnt/data"}, "/town")
	if got != "/mnt/data" {
		t.Errorf("configured path = %q, want /mnt/data", got)
	}
}

func TestIsPatrolEnabled_DiskGuard(t *testing.T) {
	if !IsPatrolEnabled(nil, "disk_guard") {
		t.Error("disk_guard must default to enabled: it is a safety rail")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{DiskGuard: &DiskGuardConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "disk_guard") {
		t.Error("disk_guard must honor an explicit enabled:false")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{DiskGuard: &DiskGuardConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "disk_guard") {
		t.Error("disk_guard must honor enabled:true")
	}
}
