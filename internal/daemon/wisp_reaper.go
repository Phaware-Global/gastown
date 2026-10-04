package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	// defaultWispReaperInterval is the patrol interval. Set to 1h since reaping
	// is cleanup work, not latency-sensitive. Was 30m before Dog-driven refactor.
	defaultWispReaperInterval = 1 * time.Hour
	// Wisps older than this are reaped (closed). Configurable via formula var max_age.
	defaultWispMaxAge = 24 * time.Hour
	// Closed wisps older than this are permanently deleted. Formula var: purge_age.
	defaultWispDeleteAge = 7 * 24 * time.Hour
	// Alert threshold: if open wisp count exceeds this, the Dog should escalate.
	// Shared with `gt reaper run` warning. See reaper.DefaultAlertThreshold.
	wispAlertThreshold = reaper.DefaultAlertThreshold
	// Closed mail older than this is permanently deleted. Formula var: mail_delete_age.
	defaultMailDeleteAge = 7 * 24 * time.Hour
	// Issues stale longer than this are auto-closed. Formula var: stale_issue_age.
	// Single source of truth: reaper.DefaultStaleIssueAge (30d), also used by
	// the `gt reaper auto-close --stale-age` CLI default and documented as the
	// mol-dog-reaper formula's default. Do NOT hardcode a different value here.
	defaultStaleIssueAge = reaper.DefaultStaleIssueAge
	// Consecutive failed Dog dispatches before the daemon escalates once.
	reaperDispatchEscalateAfter = 3
)

// WispReaperConfig holds configuration for the wisp_reaper patrol.
type WispReaperConfig struct {
	Enabled bool `json:"enabled"`
	DryRun  bool `json:"dry_run,omitempty"`
	// Destructive is the operator kill switch; only an explicit false turns it
	// on. The gt reaper write commands re-read it from daemon.json on every run,
	// so it takes effect immediately. The daemon reads daemon.json at startup, so
	// its own dispatch dry_run hint and patrol scheduling need a restart.
	Destructive      *bool    `json:"destructive,omitempty"`
	IntervalStr      string   `json:"interval,omitempty"`
	MaxAgeStr        string   `json:"max_age,omitempty"`
	DeleteAgeStr     string   `json:"delete_age,omitempty"`
	StaleIssueAgeStr string   `json:"stale_issue_age,omitempty"`
	Databases        []string `json:"databases,omitempty"`
}

// wispReaperDestructive reports whether the reaper may close or purge beads.
// It defaults to true; only an explicit destructive=false turns it off.
func wispReaperDestructive(config *WispReaperConfig) bool {
	return config == nil || config.Destructive == nil || *config.Destructive
}

// CheckReaperWritesAllowed reads mayor/daemon.json from disk and returns an
// error if the wisp_reaper patrol is switched off (enabled=false or
// destructive=false). A missing file or wisp_reaper section means no switch is
// set; an unreadable file fails closed. gt reaper calls this on every
// invocation that writes, so flipping the switch needs no restart.
func CheckReaperWritesAllowed(townRoot string) error {
	path := PatrolConfigFile(townRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read %s to check the reaper kill switch: %w", path, err)
	}
	var cfg DaemonPatrolConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("cannot parse %s to check the reaper kill switch: %w", path, err)
	}
	if cfg.Patrols == nil || cfg.Patrols.WispReaper == nil {
		return nil
	}
	wr := cfg.Patrols.WispReaper
	switch {
	case !wr.Enabled:
		return fmt.Errorf("patrols.wisp_reaper.enabled is false in %s", path)
	case !wispReaperDestructive(wr):
		return fmt.Errorf("patrols.wisp_reaper.destructive is false in %s", path)
	}
	return nil
}

// wispReaperInterval returns the configured interval, or the default (1h).
func wispReaperInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispReaperInterval
}

// wispReaperMaxAge returns the configured max age, or the default (24h).
func wispReaperMaxAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.MaxAgeStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.MaxAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispMaxAge
}

// wispDeleteAge returns the configured delete age, or the default (7 days).
func wispDeleteAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.DeleteAgeStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.DeleteAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispDeleteAge
}

// wispStaleIssueAge returns the configured stale-issue age, or the default (30 days).
func wispStaleIssueAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.StaleIssueAgeStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.StaleIssueAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultStaleIssueAge
}

// reapWisps is the thin orchestrator for the wisp_reaper patrol.
// It pours a mol-dog-reaper molecule, then dispatches a Dog to execute it.
// The Dog reads the formula steps and calls `gt reaper` CLI helpers.
// If Dog dispatch fails the cycle is skipped: the reaper closes and purges
// beads, and an unattended in-daemon run would bypass every Dog-level hold
// (gt-qlah).
func (d *Daemon) reapWisps() {
	if !d.isPatrolActive("wisp_reaper") {
		return
	}

	config := d.patrolConfig.Patrols.WispReaper
	maxAge := wispReaperMaxAge(d.patrolConfig)
	deleteAge := wispDeleteAge(d.patrolConfig)
	staleIssueAge := wispStaleIssueAge(d.patrolConfig)

	vars := map[string]string{
		"max_age":         maxAge.String(),
		"purge_age":       deleteAge.String(),
		"stale_issue_age": staleIssueAge.String(),
		"mail_delete_age": defaultMailDeleteAge.String(),
		"alert_threshold": fmt.Sprintf("%d", wispAlertThreshold),
		"dolt_port":       fmt.Sprintf("%d", d.doltServerPort()),
	}

	dryRun := config.DryRun || !wispReaperDestructive(config)
	if dryRun {
		vars["dry_run"] = "true"
	}
	if len(config.Databases) > 0 {
		vars["databases"] = strings.Join(config.Databases, ",")
	}

	// Pour the molecule for observability tracking.
	mol := d.pourDogMolecule(constants.MolDogReaper, vars)
	defer mol.close()

	if dryRun {
		d.logger.Printf("wisp_reaper: DRY RUN — reporting only, no changes will be made")
	}

	if err := d.dispatchReaperDog(vars); err != nil {
		d.logger.Printf("wisp_reaper: Dog dispatch failed (%v); skipping this cycle, no destructive steps run in the daemon", err)
		mol.failStep("scan", fmt.Sprintf("dog dispatch failed: %v", err))
		d.noteReaperDispatchFailure(err)
		return
	}
	d.reaperDispatchFailures, d.reaperDispatchEscalated = 0, false

	d.logger.Printf("wisp_reaper: dispatched to Dog for formula-driven execution")
}

// noteReaperDispatchFailure escalates once after reaperDispatchEscalateAfter
// consecutive failures; a successful dispatch re-arms it.
func (d *Daemon) noteReaperDispatchFailure(cause error) {
	d.reaperDispatchFailures++
	if d.reaperDispatchFailures < reaperDispatchEscalateAfter || d.reaperDispatchEscalated {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.gtPath, "escalate", "-s", "HIGH", //nolint:gosec // G204: d.gtPath resolved at daemon init via LookPath
		fmt.Sprintf("wisp_reaper: Dog dispatch failed %d cycles in a row, reaper not running", d.reaperDispatchFailures),
		"--reason", fmt.Sprintf("last error: %v", cause))
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("wisp_reaper: escalation failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return
	}
	d.reaperDispatchEscalated = true
}

// dispatchReaperDog dispatches the mol-dog-reaper formula to a Dog via gt sling.
func (d *Daemon) dispatchReaperDog(vars map[string]string) error {
	args := []string{"sling", constants.MolDogReaper, "deacon/dogs"}
	for k, v := range vars {
		args = append(args, "--var", fmt.Sprintf("%s=%s", k, v))
	}

	cmd := exec.Command(d.gtPath, args...) //nolint:gosec // G204: d.gtPath resolved at daemon init via LookPath
	cmd.Dir = d.config.TownRoot
	// Inherit os.Environ() (cmd.Env left nil) — gt sling performs WRITES
	// (creates wisps, dispatches dogs) so it must NOT carry
	// BD_DOLT_AUTO_COMMIT=off from bdReadOnlyEnv(). PATH augmentation at
	// daemon startup (PATCH-007) ensures the inherited env still finds
	// gt/bd via os.Environ()'s PATH.
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gt sling: %w", err)
	}
	return nil
}

// doltServerPort returns the configured Dolt server port.
func (d *Daemon) doltServerPort() int {
	if d.doltServer != nil {
		return d.doltServer.config.Port
	}
	return 3307
}
