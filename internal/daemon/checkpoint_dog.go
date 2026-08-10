package daemon

import (
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	defaultCheckpointDogInterval = 10 * time.Minute
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
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
		if alive, err := d.tmux.HasSession(sessionName); err != nil {
			d.logger.Printf("checkpoint_dog: error checking session %s: %v", sessionName, err)
		} else if !alive {
			d.logger.Printf("checkpoint_dog: session %s is dead — checkpointing anyway (highest-risk case)", sessionName)
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
		// (gt-checkpoint-workdir fix.)
		workDir := resolveCheckpointWorkDir(polecatsDir, polecatName, rigName)
		if workDir == "" {
			continue // Neither layout has a usable .git — skip silently.
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
		// human resolves the hook failure; the work itself is not at risk.
		d.logger.Printf("checkpoint_dog: ERROR — %s/%s checkpoint committed locally but its pre-commit hook FAILED, so it was NOT pushed: %s", rigName, polecatName, result.HookOutput)
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
