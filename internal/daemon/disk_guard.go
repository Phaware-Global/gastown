package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

	// DiskGuardCriticalRepeat is how often a CRITICAL alert repeats while its
	// episode lasts. HIGH never repeats.
	DiskGuardCriticalRepeat = 15 * time.Minute

	// DiskGuardMaxAttempts is how many times one alert is attempted before the
	// guard logs an error and stops until the next scheduled repeat or episode.
	DiskGuardMaxAttempts = 3

	// diskGuardRecoveryMargin is how far above a threshold free space must
	// climb for an episode to end. It keeps a volume that hovers at the
	// threshold (build output created and deleted, or macOS purgeable space
	// shifting) from starting a new episode on every dip.
	diskGuardRecoveryMargin = 1.1

	// diskGuardEscalateGrace is how long a timed-out gt escalate session gets
	// after SIGTERM before SIGKILL.
	diskGuardEscalateGrace = 3 * time.Second

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

// Disk-guard alert contract
//
//  1. Episodes. An episode for a path and a severity starts when free space
//     falls below that severity's threshold and ends when it recovers above
//     the threshold plus a 10% margin. A new dip after recovery is a new
//     episode. Changing the watched path starts fresh episodes.
//  2. Alerts. One escalation when an episode starts. CRITICAL repeats every
//     15 minutes while free space stays below the critical threshold (the
//     repeat is deliberate: it is also what recovers a lost alert). HIGH never
//     repeats. A delivered CRITICAL covers HIGH for the same stretch, so
//     dropping from critical back to the warn range is not announced again.
//     If CRITICAL gave up undelivered, nobody may know, so a HIGH episode that
//     starts or continues after that still alerts.
//  3. Delivery. Bounded duplicates are acceptable; a lost alert is not. There
//     is no duplicate-suppression key and no "suppressed" exit path. An alert
//     counts as sent only after gt escalate exits 0. Any failure (non-zero
//     exit or timeout) is retried on the next heartbeat, at most 3 attempts
//     per alert; after that the guard logs an error naming the path and
//     severity and stays quiet until the next scheduled CRITICAL repeat or the
//     next episode.
//  4. No orphans. gt runs in its own session and a timeout signals every
//     process in it, including the bd children gt starts in their own process
//     groups. gt's bd calls get a timeout below the outer one, so they end on
//     their own first.

// diskEpisode is one severity's alert state for the path being watched.
type diskEpisode struct {
	active   bool      // free space is below this severity's threshold (episode in progress)
	due      time.Time // earliest time the next attempt may run; zero = nothing scheduled
	attempts int       // failed attempts of the current alert
	alertAt  time.Time // when the current alert was first attempted
}

// diskGuard escalates when free space on a volume drops below configured
// thresholds, following the contract above. It holds only the per-episode
// alert state; the free-space reader and the escalator are injected so tests
// need neither a real filesystem nor a gt binary.
type diskGuard struct {
	config *DiskGuardConfig

	freeBytes func(path string) (uint64, error)
	escalate  func(severity, message string) error
	logf      func(format string, args ...interface{})

	path string
	warn diskEpisode
	crit diskEpisode
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

// check reads free space at path and sends whatever alert the contract calls
// for. An alert is recorded as sent only after the escalation succeeds.
func (g *diskGuard) check(path string, now time.Time) {
	free, err := g.freeBytes(path)
	if err != nil {
		// An unreadable path says nothing about how full the disk is.
		g.logf("disk_guard: cannot read free space at %s: %v", path, err)
		return
	}

	if path != g.path {
		g.path, g.warn, g.crit = path, diskEpisode{}, diskEpisode{}
	}

	warn, critical := g.thresholds()
	freeGB := float64(free) / float64(1<<30)

	for _, e := range []struct {
		ep    *diskEpisode
		limit float64
	}{{&g.warn, warn}, {&g.crit, critical}} {
		switch {
		case freeGB >= e.limit*diskGuardRecoveryMargin:
			*e.ep = diskEpisode{}
		case freeGB < e.limit && !e.ep.active:
			*e.ep = diskEpisode{active: true, due: now}
		}
	}

	severity, limit, ep := "HIGH", warn, &g.warn
	switch {
	case freeGB < critical:
		severity, limit, ep = "CRITICAL", critical, &g.crit
	case freeGB >= warn:
		return
	}
	if !ep.active || ep.due.IsZero() || now.Before(ep.due) {
		return
	}

	msg := fmt.Sprintf("Disk space low: %s free at %s (below %.0f GiB). "+
		"Dolt cannot write once the volume is full; free space now "+
		"(stale build output in polecat scratch directories is the usual culprit).",
		util.FormatBytesHuman(free), path, limit)
	g.logf("disk_guard: %s: %s", severity, msg)

	if ep.attempts == 0 {
		ep.alertAt = now
	}
	if err := g.escalate(severity, msg); err != nil {
		ep.attempts++
		if ep.attempts < DiskGuardMaxAttempts {
			g.logf("disk_guard: %s escalation for %s not delivered (attempt %d of %d), retrying next heartbeat: %v",
				severity, path, ep.attempts, DiskGuardMaxAttempts, err)
			return
		}
		ep.attempts = 0
		ep.due = time.Time{}
		next := "the next episode"
		if severity == "CRITICAL" {
			ep.due = ep.alertAt.Add(DiskGuardCriticalRepeat)
			next = "the next scheduled repeat"
		}
		g.logf("disk_guard: ERROR: giving up on %s alert for %s after %d failed attempts; no further attempts until %s: %v",
			severity, path, DiskGuardMaxAttempts, next, err)
		return
	}

	ep.attempts = 0
	if severity == "CRITICAL" {
		ep.due = now.Add(DiskGuardCriticalRepeat)
		// A delivered CRITICAL covers HIGH while free space is still low.
		g.warn.due, g.warn.attempts = time.Time{}, 0
	} else {
		ep.due = time.Time{}
	}
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
			logf: d.logger.Printf,
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
	d.diskGuard.escalate = d.escalateDiskFree
	d.diskGuard.check(path, time.Now())
}

// diskGuardEscalateArgs builds the gt escalate argument list for a disk alert.
// It deliberately carries no --fingerprint: a duplicate-suppressed exit 0 would
// count as delivered even when the original attempt wrote the bead but never
// sent mail, and a lost alert is worse than a bounded duplicate.
func diskGuardEscalateArgs(severity, message string) []string {
	return []string{"escalate", "-s", severity, "Disk: " + message}
}

// diskGuardBdTimeoutSec is the timeout handed to the bd calls gt makes: half
// the outer timeout, so a hung bd ends on its own before the outer kill.
func diskGuardBdTimeoutSec(timeout time.Duration) int {
	if n := int(timeout / 2 / time.Second); n > 1 {
		return n
	}
	return 1
}

// runDiskGuardEscalation runs gt with args in dir, bounded by timeout. Any
// non-zero exit or deadline is an error.
func runDiskGuardEscalation(gt, dir string, args []string, timeout time.Duration, logf func(string, ...interface{})) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, gt, args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"BD_ACTOR=daemon",
		"GT_BD_TIMEOUT_SEC="+strconv.Itoa(diskGuardBdTimeoutSec(timeout)))
	// gt starts each bd in its own process group, so a group kill of gt alone
	// would orphan a hung bd: run gt in its own session and signal all of it.
	setEscalationSession(cmd, diskGuardEscalateGrace)
	// WaitDelay is the backstop that closes the output pipes if a descendant
	// outlives the signal.
	cmd.WaitDelay = 5 * time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		logf("disk_guard: escalation failed: %v (%s)", err, strings.TrimSpace(string(output)))
		return err
	}
	return nil
}

// escalateDiskFree files an escalation via gt escalate at the given severity.
func (d *Daemon) escalateDiskFree(severity, message string) error {
	gt := d.gtPath
	if gt == "" {
		gt = "gt"
	}
	return runDiskGuardEscalation(gt, d.config.TownRoot,
		diskGuardEscalateArgs(severity, message), diskGuardEscalateTimeout, d.logger.Printf)
}
