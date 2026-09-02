package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	defaultCheckpointDogInterval = 10 * time.Minute

	// checkpointDogAbandonedThreshold bounds how long a dead session's
	// worktree keeps getting auto-checkpointed after its last known
	// activity. Distinct from (and much larger than) the session-liveness
	// heartbeat threshold: checkpointing a session that just died is the
	// highest-risk case this dog exists for, but a worktree whose session
	// has been dead well beyond this window is more likely a deliberately
	// abandoned or gone-wrong worktree than one about to be reassigned,
	// and should stop being auto-published (PR #184 review).
	checkpointDogAbandonedThreshold = 24 * time.Hour
)

// CheckpointDogConfig holds configuration for the checkpoint_dog patrol.
type CheckpointDogConfig struct {
	// Enabled controls whether the checkpoint dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "10m").
	IntervalStr string `json:"interval,omitempty"`
}

// checkpointDogInterval returns the configured interval, or the default (10m).
func checkpointDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.CheckpointDog != nil {
		if config.Patrols.CheckpointDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.CheckpointDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultCheckpointDogInterval
}

// runCheckpointDog auto-commits WIP changes in active polecat worktrees.
// This protects against data loss when sessions crash or hit context limits.
//
// ## ZFC Exemption
// The checkpoint dog executes git operations directly via internal/git (same
// pattern as compactor_dog's SQL operations). The daemon pours a molecule for
// observability, then delegates staging/commit/push to git.AutoPreserveUncommittedWork.
func (d *Daemon) runCheckpointDog() {
	if !d.isPatrolActive("checkpoint_dog") {
		return
	}

	d.logger.Printf("checkpoint_dog: starting cycle")

	mol := d.pourDogMolecule(constants.MolDogCheckpoint, nil)
	defer mol.close()

	rigs := d.getKnownRigs()
	totalScanned := 0
	totalCheckpointed := 0

	for _, rigName := range rigs {
		scanned, checkpointed := d.checkpointRigPolecats(rigName)
		totalScanned += scanned
		totalCheckpointed += checkpointed
	}

	mol.closeStep("scan")
	mol.closeStep("checkpoint")

	d.logger.Printf("checkpoint_dog: cycle complete — scanned %d worktrees, checkpointed %d",
		totalScanned, totalCheckpointed)
	mol.closeStep("report")
}

// checkpointRigPolecats checkpoints dirty polecat worktrees in a single rig.
// Returns (scanned, checkpointed) counts.
func (d *Daemon) checkpointRigPolecats(rigName string) (int, int) {
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		return 0, 0
	}

	scanned := 0
	checkpointed := 0

	for _, polecatName := range polecats {
		if d.isShutdownInProgress() {
			d.logger.Printf("checkpoint_dog: shutdown in progress, aborting %s walk (%d/%d polecats scanned)", rigName, scanned, len(polecats))
			break
		}
		scanned++

		// Deliberately checkpoint regardless of whether the tmux session is
		// alive. This dog exists to protect against data loss when a session
		// crashes — a dead session is exactly the case that needs it most,
		// not a reason to skip: it is the one polecat state nothing else is
		// actively preserving, and the one most likely to be reassigned or
		// nuked next (gt-y8ts). The previous "alive sessions only" gate is
		// why the checkpoint fired for one incident polecat but not for two
		// others in the same afternoon — both had already died by the time
		// this patrol ran.
		//
		// That said, this must not turn into "every worktree on the box,
		// indefinitely" (PR #184 review). A tmux failure means we can't
		// tell whether the worktree is even in scope, so skip it for
		// safety rather than checkpointing blind — the next cycle will
		// retry. And a session dead well beyond checkpointDogAbandonedThreshold
		// is more likely a deliberately-left-dirty or gone-wrong worktree
		// than one about to be reassigned, so stop auto-publishing it.
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
		alive, err := d.tmux.HasSession(sessionName)
		if err != nil {
			d.logger.Printf("checkpoint_dog: error checking session %s, skipping for safety: %v", sessionName, err)
			continue
		}
		// Polecat layout: prefer <polecatsDir>/<name>/<rigName>/ (the new
		// nested layout where the outer <name>/ dir is a container with
		// per-polecat scaffolding and the inner dir is the actual git
		// worktree). Fall back to <polecatsDir>/<name>/ for the legacy
		// flat layout still supported by polecat.Manager. Both candidates
		// must contain `.git` — never fall back to a parent dir, since
		// the original bug here was exactly that: an empty <name>/
		// container caused git to walk up to the top-level workspace's
		// .git and commit "WIP: checkpoint (auto)" on the workspace's
		// branch (usually main) instead of the polecat's branch.
		// (gt-checkpoint-workdir fix.) Resolved before the alive check
		// below so it can double as the abandoned-worktree age fallback.
		workDir := resolveCheckpointWorkDir(polecatsDir, polecatName, rigName)
		if workDir == "" {
			continue // Neither layout has a usable .git — skip silently.
		}

		if !alive {
			// Bound how long a dead session's worktree keeps getting
			// auto-checkpointed. Prefer the session heartbeat, but a clean
			// session teardown deletes exactly that file — killIdlePolecat
			// and ReconcilePool both call RemoveSessionHeartbeat right
			// after reaping a session — so a missing heartbeat means
			// "reaped (or never had one)," not "just started." Treating a
			// missing heartbeat as fresh inverted this bound: it fired for
			// a session that crashed seconds ago and never fired for one
			// cleanly reaped weeks ago — the actual "deliberately
			// abandoned" case the threshold exists to catch (PR #184
			// review). Fall back to the worktree's own activity signals
			// (git index mtime, HEAD commit date — see
			// checkpointDogWorktreeAge), which survive session reaping.
			age, ageErr := checkpointDogWorktreeAge(workDir, d.config.TownRoot, sessionName)
			if ageErr == nil && age > checkpointDogAbandonedThreshold {
				d.logger.Printf("checkpoint_dog: session %s dead, last activity %s ago (> %s) — skipping as abandoned", sessionName, age.Round(time.Minute), checkpointDogAbandonedThreshold)
				continue
			}
			d.logger.Printf("checkpoint_dog: session %s is dead — checkpointing anyway (highest-risk case)", sessionName)
		}

		if d.checkpointWorktree(workDir, rigName, polecatName) {
			checkpointed++
		}
	}

	return scanned, checkpointed
}

// checkpointWorktree creates a WIP checkpoint for a single worktree and
// pushes it to a preservation ref. Returns true if anything was preserved
// (a new commit, a push of prior unpushed commits, or both).
//
// Delegates to git.AutoPreserveUncommittedWork — the same implementation gt
// done's gt-pvx safety net and polecat removal use — rather than reimplementing
// staging/exclusion/commit logic a third time (gt-y8ts). This also fixes a
// policy drift bug: the old hand-rolled exclusion list here
// (.claude/.beads/.runtime/__pycache__) diverged from gt done's broader
// runtime-artifact policy (also excludes .opencode, .logs, node_modules,
// .vite, language caches, CLAUDE.local.md, .DS_Store, .db/.pyc/.pyo), so a
// checkpoint could commit files gt done would have excluded.
//
// Unlike gt done, this pushes to a dedicated polecat/preserve-<branch> ref rather
// than the branch itself: a checkpoint can fire while a PR is already open
// on the branch, and force-pushing WIP commits onto it would disrupt review
// and CI. Verified against the remote tip, since nothing else pushes on
// this path before the worktree might be reassigned or removed.
func (d *Daemon) checkpointWorktree(workDir, rigName, polecatName string) bool {
	g := git.NewGit(workDir)
	branch, err := g.CurrentBranch()
	if err != nil || branch == "" {
		d.logger.Printf("checkpoint_dog: cannot read HEAD in %s/%s: %v — skipping for safety", rigName, polecatName, err)
		return false
	}

	result, err := git.AutoPreserveUncommittedWork(g, branch, git.PreserveOptions{
		IssueID:       polecatName,
		Push:          true,
		CommitMessage: checkpoint.WIPCommitPrefix,
	})
	if err != nil {
		// Covers the G41 protected-branch refusal, unmerged-conflict refusal,
		// and any git-operation failure — all deliberately non-fatal here so
		// one broken worktree never stops the patrol from checkpointing the
		// rest. Work stays uncommitted in the worktree for manual recovery.
		d.logger.Printf("checkpoint_dog: could not checkpoint %s/%s: %v", rigName, polecatName, err)
		return false
	}

	if !result.Committed && !result.Pushed {
		return false // clean worktree, nothing to preserve
	}
	if result.Committed {
		d.logger.Printf("checkpoint_dog: created WIP checkpoint in %s/%s", rigName, polecatName)
	}
	if result.HooksFailed {
		// The commit exists locally but was never verified — pushing it
		// would defeat the hooks (gt-i4ej FIX 2). Escalate loudly so a
		// human resolves the hook failure — but do not copy the hook's
		// stderr into this log (HookOutput is now redacted at the source
		// for the same reason, but this site keeps its own wording): the
		// hook this gate exists for is typically a secret scanner, so its
		// verbatim output can contain the very credential it caught.
		// Writing that into the daemon's shared, rotated, gzip-archived
		// log would relocate the secret to a longer-lived, more
		// widely-read artifact than the uncommitted file it came from
		// (PR #184 review). Point at the worktree instead.
		d.logger.Printf("checkpoint_dog: ERROR — %s/%s checkpoint committed locally but its pre-commit hook FAILED, so it was NOT pushed. Run `git commit` in %s to see the hook output.", rigName, polecatName, workDir)
	}
	if result.Pushed {
		d.logger.Printf("checkpoint_dog: pushed %s/%s checkpoint to %s (%s)", rigName, polecatName, result.Ref, result.Commit)
	}
	return true
}

// isGitWorktree reports whether the given directory is the root of a git
// worktree (has its own `.git` file or directory). Used to guard checkpoint
// commits against the "wrong-dir" failure mode where git operations in a
// non-worktree directory walk up the filesystem tree and commit on the
// parent workspace's branch.
func isGitWorktree(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// resolveCheckpointWorkDir picks the actual git-worktree directory for a
// polecat, supporting both the new nested layout (polecats/<name>/<rigName>/)
// and the legacy flat layout (polecats/<name>/) that polecat.Manager still
// recognizes for backward compatibility. Returns "" if neither candidate is
// a git worktree, in which case the caller MUST skip the polecat — never
// fall back to a parent directory, since git would walk up to the top-level
// workspace's .git and commit on the wrong branch (this is the bug this
// helper exists to prevent).
func resolveCheckpointWorkDir(polecatsDir, polecatName, rigName string) string {
	nested := filepath.Join(polecatsDir, polecatName, rigName)
	if isGitWorktree(nested) {
		return nested
	}
	flat := filepath.Join(polecatsDir, polecatName)
	if isGitWorktree(flat) {
		return flat
	}
	return ""
}

// checkpointDogWorktreeAge returns how long ago a dead-session worktree was
// last active, for the checkpointDogAbandonedThreshold bound. Takes the most
// recent of every available signal rather than the first one that resolves:
//
//   - the session heartbeat (deleted by a clean reap, so its absence means
//     "reaped or never had one", not "fresh");
//   - the git index mtime — a signal of THIS worktree's activity that
//     survives reaping. HEAD's commit date alone is NOT such a signal: a
//     polecat created from the current base-branch tip whose session died
//     before its first commit inherits the base branch's last-commit date,
//     so on any repo whose default branch hadn't moved in 24h every
//     cleanly-reaped, not-yet-committed polecat — the population this dog
//     exists for — was classified abandoned (PR #184 review);
//   - HEAD's commit date, as the floor for repos where the index can't be
//     statted.
//
// Erring fresh (preserving too often) is the safe direction; the caller
// also treats an error here as "not abandoned" for the same reason.
func checkpointDogWorktreeAge(workDir, townRoot, sessionName string) (time.Duration, error) {
	var newest time.Time
	if hb := polecat.ReadSessionHeartbeat(townRoot, sessionName); hb != nil && hb.Timestamp.After(newest) {
		newest = hb.Timestamp
	}
	g := git.NewGit(workDir)
	if mt, err := g.IndexModTime(); err == nil && mt.After(newest) {
		newest = mt
	}
	if ct, err := g.LastActivityTime(); err == nil && ct.After(newest) {
		newest = ct
	}
	if newest.IsZero() {
		return 0, fmt.Errorf("no activity signal available for %s", workDir)
	}
	return time.Since(newest), nil
}
