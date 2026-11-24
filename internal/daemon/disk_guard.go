package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// Disk-guard defaults. Free space is measured in GiB on the volume holding the
// Dolt data directory: a full data volume makes Dolt die mid-write, after which
// every bd/gt write fails and nothing can even file an escalation. The warning
// has to fire while there is still room to act.
const (
	// DefaultDiskWarnFreeGB is the free-space level below which the guard
	// escalates a HIGH warning.
	DefaultDiskWarnFreeGB = 30.0

	// DefaultDiskCriticalFreeGB is the free-space level below which the guard
	// escalates CRITICAL.
	DefaultDiskCriticalFreeGB = 10.0

	// DiskGuardWarnCooldown rate-limits repeat HIGH escalations.
	DiskGuardWarnCooldown = time.Hour

	// DiskGuardCriticalCooldown rate-limits repeat CRITICAL escalations.
	DiskGuardCriticalCooldown = 15 * time.Minute

	// diskGuardRecoveryMargin is how far above the warn threshold free space
	// must climb before the escalation throttle clears. It keeps a volume that
	// hovers at the threshold (build output created and deleted, or macOS
	// purgeable space shifting) from re-escalating on every dip.
	diskGuardRecoveryMargin = 1.1

	// diskGuardEscalateTimeout bounds the gt escalate subprocess; it runs inline
	// on the heartbeat path.
	diskGuardEscalateTimeout = 20 * time.Second
)

type diskLevel int

const (
	diskOK diskLevel = iota
	diskWarn
	diskCritical
)

// diskGuard escalates when free space on a volume drops below configured
// thresholds. It holds only the escalation-throttling state; the free-space
// reader and the escalator are injected so tests need neither a real
// filesystem nor a gt binary.
type diskGuard struct {
	config *DiskGuardConfig

	freeBytes func(path string) (uint64, error)
	escalate  func(severity, message string) error
	logf      func(format string, args ...interface{})

	// lastWarnAt and lastCriticalAt are when an escalation of that severity
	// was last delivered; zero means none is outstanding.
	lastWarnAt     time.Time
	lastCriticalAt time.Time
}

// thresholds returns the effective (warn, critical) free-space limits in GiB.
// Non-positive values, or a critical limit not below the warn limit, are
// configuration mistakes; they fall back to the defaults rather than silently
// disabling or inverting the guard.
func (g *diskGuard) thresholds() (warn, critical float64) {
	warn, critical = DefaultDiskWarnFreeGB, DefaultDiskCriticalFreeGB
	if g.config == nil {
		return warn, critical
	}
	w, c := g.config.WarnFreeGB, g.config.CriticalFreeGB
	if w > 0 {
		warn = w
	}
	if c > 0 {
		critical = c
	}
	if critical >= warn {
		return DefaultDiskWarnFreeGB, DefaultDiskCriticalFreeGB
	}
	return warn, critical
}

// check reads free space at path and escalates if it is below a threshold.
//
// Throttling is per severity: a HIGH is suppressed for DiskGuardWarnCooldown
// after any delivered escalation, a CRITICAL for DiskGuardCriticalCooldown
// after a delivered CRITICAL. A first CRITICAL is therefore never held back by
// an earlier HIGH. The throttle clears only once free space is back above the
// warn threshold plus diskGuardRecoveryMargin, so a volume hovering at a
// threshold does not escalate on every dip. Throttle state is committed only
// after the escalation is delivered; a failed one is retried on the next tick.
func (g *diskGuard) check(path string, now time.Time) {
	free, err := g.freeBytes(path)
	if err != nil {
		// An unreadable path says nothing about how full the disk is.
		g.logf("disk_guard: cannot read free space at %s: %v", path, err)
		return
	}

	warn, critical := g.thresholds()
	freeGB := float64(free) / float64(1<<30)

	level := diskOK
	switch {
	case freeGB < critical:
		level = diskCritical
	case freeGB < warn:
		level = diskWarn
	}

	if level == diskOK {
		if freeGB >= warn*diskGuardRecoveryMargin {
			g.lastWarnAt = time.Time{}
			g.lastCriticalAt = time.Time{}
		}
		return
	}

	severity, limit := "HIGH", warn
	if level == diskCritical {
		severity, limit = "CRITICAL", critical
	}
	if g.throttled(level, now) {
		return
	}

	msg := fmt.Sprintf("Disk space low: %s free at %s (below %.0f GiB). "+
		"Dolt cannot write once the volume is full; free space now "+
		"(stale build output in polecat scratch directories is the usual culprit).",
		util.FormatBytesHuman(free), path, limit)
	g.logf("disk_guard: %s: %s", severity, msg)
	if err := g.escalate(severity, msg); err != nil {
		g.logf("disk_guard: %s escalation not delivered, will retry next tick: %v", severity, err)
		return
	}
	if level == diskCritical {
		g.lastCriticalAt = now
	} else {
		g.lastWarnAt = now
	}
}

// throttled reports whether an escalation at level is still inside the
// cooldown of a delivered one. A delivered CRITICAL also quiets HIGH: dropping
// from critical to warn is an improvement, not news.
func (g *diskGuard) throttled(level diskLevel, now time.Time) bool {
	within := func(at time.Time, cooldown time.Duration) bool {
		return !at.IsZero() && now.Sub(at) < cooldown
	}
	if level == diskCritical {
		return within(g.lastCriticalAt, DiskGuardCriticalCooldown)
	}
	return within(g.lastWarnAt, DiskGuardWarnCooldown) || within(g.lastCriticalAt, DiskGuardWarnCooldown)
}

// resolveDiskGuardPath returns the path whose volume is checked: the
// configured path, else the managed Dolt server's data directory, else the
// conventional <town>/.dolt-data. Only the conventional default falls back to
// the town root when it does not exist (default layout: same volume). A
// configured or managed path is returned as-is even if missing, so the guard
// keeps naming it and logs the read failure instead of silently watching a
// different disk.
func resolveDiskGuardPath(cfg *DiskGuardConfig, managedDataDir, townRoot string, exists func(string) bool) string {
	if cfg != nil && cfg.Path != "" {
		return cfg.Path
	}
	if managedDataDir != "" {
		return managedDataDir
	}
	path := filepath.Join(townRoot, ".dolt-data")
	if !exists(path) {
		return townRoot
	}
	return path
}

// checkDiskFree is the heartbeat hook for the disk guard.
func (d *Daemon) checkDiskFree() {
	var cfg *DiskGuardConfig
	if d.patrolConfig != nil && d.patrolConfig.Patrols != nil {
		cfg = d.patrolConfig.Patrols.DiskGuard
	}

	if d.diskGuard == nil {
		d.diskGuard = &diskGuard{
			freeBytes: func(path string) (uint64, error) {
				info, err := util.GetDiskSpace(path)
				if err != nil {
					return 0, err
				}
				return info.AvailableBytes, nil
			},
			escalate: d.escalateDiskFree,
			logf:     d.logger.Printf,
		}
	}
	d.diskGuard.config = cfg

	managed := ""
	if d.doltServer != nil && d.doltServer.IsEnabled() {
		managed = d.doltServer.config.DataDir
	}
	path := resolveDiskGuardPath(cfg, managed, d.config.TownRoot, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	d.diskGuard.check(path, time.Now())
}

// escalateDiskFree files an escalation via gt escalate at the given severity.
func (d *Daemon) escalateDiskFree(severity, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), diskGuardEscalateTimeout)
	defer cancel()

	gt := d.gtPath
	if gt == "" {
		gt = "gt"
	}
	cmd := exec.CommandContext(ctx, gt, "escalate", "-s", severity, "Disk: "+message) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)
	// The context deadline alone does not bound a child whose grandchild
	// (bd, dolt) keeps the output pipe open; WaitDelay does.
	cmd.WaitDelay = 5 * time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("disk_guard: escalation failed: %v (%s)", err, strings.TrimSpace(string(output)))
		return err
	}
	return nil
}
