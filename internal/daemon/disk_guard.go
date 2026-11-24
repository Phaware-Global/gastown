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

	lastLevel     diskLevel
	lastEscalated time.Time
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
// Repeats are suppressed for a cooldown per severity, but a worsening level
// (warn -> critical) escalates immediately, and a recovery resets the
// throttle so a later dip is reported at once.
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
		g.lastLevel = diskOK
		g.lastEscalated = time.Time{}
		return
	}

	cooldown := DiskGuardWarnCooldown
	severity := "HIGH"
	limit := warn
	if level == diskCritical {
		cooldown = DiskGuardCriticalCooldown
		severity = "CRITICAL"
		limit = critical
	}

	worsened := level > g.lastLevel
	g.lastLevel = level
	if !worsened && !g.lastEscalated.IsZero() && now.Sub(g.lastEscalated) < cooldown {
		return
	}
	g.lastEscalated = now

	msg := fmt.Sprintf("Disk space low: %s free at %s (below %.0f GiB). "+
		"Dolt cannot write once the volume is full; free space now "+
		"(stale build output in polecat scratch directories is the usual culprit).",
		util.FormatBytesHuman(free), path, limit)
	g.logf("disk_guard: %s: %s", severity, msg)
	_ = g.escalate(severity, msg)
}

// resolveDiskGuardPath returns the path whose volume is checked.
func resolveDiskGuardPath(cfg *DiskGuardConfig, managedDataDir, townRoot string, exists func(string) bool) string {
	if cfg != nil && cfg.Path != "" {
		return cfg.Path
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

	path := resolveDiskGuardPath(cfg, "", d.config.TownRoot, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	d.diskGuard.check(path, time.Now())
}

// escalateDiskFree files an escalation via gt escalate at the given severity.
func (d *Daemon) escalateDiskFree(severity, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), diskGuardEscalateTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gt", "escalate", "-s", severity, "Disk: "+message)
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("disk_guard: escalation failed: %v (%s)", err, strings.TrimSpace(string(output)))
		return err
	}
	return nil
}
