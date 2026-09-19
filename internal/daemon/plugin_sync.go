package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/plugin"
)

// pluginSyncGitTimeout bounds a single runPluginSync cycle's git work
// (fetch + rev-parse + archive). Tied to d.ctx so daemon shutdown cancels an
// in-flight sync immediately rather than waiting out the full timeout.
const pluginSyncGitTimeout = 3 * time.Minute

// defaultPluginSyncInterval controls how often the daemon re-deploys
// <townRoot>/plugins from the gastown repo's plugins/ directory at
// origin/main.
const defaultPluginSyncInterval = 30 * time.Minute

// pluginSyncInterval returns the configured plugin sync interval, or the
// default (30m).
func pluginSyncInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.PluginSync != nil {
		if config.Patrols.PluginSync.Interval != "" {
			if d, err := time.ParseDuration(config.Patrols.PluginSync.Interval); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultPluginSyncInterval
}

// syncedPlugins is the explicit allowlist of plugins whose deployed copy is
// kept in sync from origin/main. Deliberately NOT "every plugin under
// plugins/" — a town-wide audit for gt-2ea1 found several plugins whose
// deployed copy has drifted in the OTHER direction (a hand-applied
// production fix, newer than anything committed to the repo, e.g.
// compactor-dog and git-hygiene carry run.sh.bak-mayor-* backups from
// in-place hotfixes that were never backported). Blindly syncing those from
// the repo would silently regress production to older, unfixed code. Only
// add a plugin here after confirming its production copy has no
// uncommitted fix — see gt-bpew for the full audit and remaining plugins.
//
// Currently empty: dolt-archive was the intended first entry (gt-2ea1), but
// its own deployed copy turned out to have the same reverse-drift problem —
// PROD_DBS narrowed to ("hq") because "gt"/"mo" were never real database
// names, a bd export invocation fixed to use -C with the right rig root
// instead of a nonexistent --db name flag, and a "KNOWN ISSUE: do not
// escalate (hq-wisp-2egq1)" block in plugin.md — none of which are on
// origin/main (PR #236 review). Syncing origin/main over it would silently
// revert those fixes on the very first daemon tick. Add "dolt-archive" back
// here only once that gap is backported to main as real commits.
var syncedPlugins = []string{}

// runPluginSync keeps syncedPlugins in <townRoot>/plugins matched to the
// gastown repo's plugins/ tree at origin/main. Non-fatal: errors are logged
// but don't stop the daemon.
//
// This closes the deployment gap behind gt-2ea1: previously nothing ever
// re-synced the deployed plugins directory after its first manual copy, so
// dolt-archive ran a 3-month-stale version with none of the fixes merged to
// main since. SyncFromOrigin reads straight from a gastown checkout's git
// object store at origin/main, so it stays correct even when every on-disk
// checkout (including this one) has drifted onto a stale or unrelated
// branch.
func (d *Daemon) runPluginSync() {
	if !d.isPatrolActive("plugin_sync") {
		return
	}
	if len(syncedPlugins) == 0 {
		// Nothing allowlisted yet — see syncedPlugins' doc comment. Skip
		// silently rather than calling SyncFromOrigin just to hit its
		// empty-list error every cycle.
		return
	}

	townRoot := d.config.TownRoot
	gitDir, err := plugin.FindGastownGitDir(townRoot)
	if err != nil {
		d.logger.Printf("plugin_sync: %v, skipping", err)
		return
	}

	ctx, cancel := context.WithTimeout(d.ctx, pluginSyncGitTimeout)
	defer cancel()

	targetDir := filepath.Join(townRoot, "plugins")
	result, err := plugin.SyncFromOrigin(ctx, gitDir, targetDir, syncedPlugins, false)
	if err != nil {
		d.logger.Printf("plugin_sync: sync from %s failed: %v", gitDir, err)
		return
	}

	if len(result.Copied) > 0 {
		d.logger.Printf("plugin_sync: deployed %d plugin(s) from origin/main: %v", len(result.Copied), result.Copied)
	}
	for _, e := range result.Errors {
		d.logger.Printf("plugin_sync: error: %s", e)
	}
}
