package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// isDefaultBranch reports whether branch is one a polecat must never work on
// directly. Polecats commit on polecat/<name>-<bead> branches and submit through
// the merge queue / PR path; main and master are owned by the refinery. (gt-tk5)
func isDefaultBranch(branch string) bool {
	switch strings.TrimSpace(branch) {
	case "main", "master":
		return true
	default:
		return false
	}
}

// ensurePolecatOffMain refuses to let a polecat session proceed while its
// worktree HEAD sits on main/master, auto-restoring it to the namespaced work
// branch (polecat/<name>-<bead>) when it has enough information to do so.
//
// This is the "block work on main" half of the defense gt-i71 started with
// "block push to main". Incident (gt-tk5): the gt-pvx auto-save hook left a
// polecat worktree on main; when the polecat was re-dispatched it was wedged —
// the tap-guard blocks `git checkout -b`, so it could neither start work nor
// run `gt done`, and a human had to hand-create the branch. Detecting the
// off-main state at session start and restoring it closes that hole.
//
// Returns nil when it is safe to continue: not a polecat, already off main, or
// successfully restored. Returns an error when the session must NOT proceed —
// the caller surfaces it and exits non-zero so the operator/witness sees a
// wedged worktree instead of a polecat silently committing onto main.
//
// In --dry-run the guard only reports what it would do; it never mutates git.
func ensurePolecatOffMain(ctx RoleContext, hookedBead *beads.Issue) error {
	if ctx.Role != RolePolecat {
		return nil
	}

	g := git.NewGit(ctx.WorkDir)
	branch, err := g.CurrentBranch()
	if err != nil {
		// Can't read the branch (not a git dir, transient git error). Don't
		// wedge prime over a read failure — the commit/push guards (gt-pvx,
		// gt-i71) still backstop main if work somehow lands there.
		explain(true, fmt.Sprintf("Off-main guard: skipped (cannot read branch: %v)", err))
		return nil
	}
	branch = strings.TrimSpace(branch)
	if !isDefaultBranch(branch) {
		explain(true, fmt.Sprintf("Off-main guard: worktree on %q — ok", branch))
		return nil
	}

	polecatName := strings.TrimSpace(ctx.Polecat)
	if polecatName == "" {
		polecatName = strings.TrimSpace(os.Getenv("GT_POLECAT"))
	}
	var beadID string
	if hookedBead != nil {
		beadID = strings.TrimSpace(hookedBead.ID)
	}

	// Dry-run / state inspection must never mutate git.
	if primeDryRun {
		explain(true, fmt.Sprintf("Off-main guard: worktree on %q — would restore to polecat/%s-%s",
			branch, polecatName, beadID))
		return nil
	}

	// Without both halves we can't name the work branch → genuinely stuck.
	// Refuse loudly and direct the operator at the recovery affordance.
	if polecatName == "" || beadID == "" {
		printOffMainRefusal(branch, polecatName, beadID)
		return fmt.Errorf("polecat worktree is on %q and cannot be auto-restored "+
			"(polecat=%q bead=%q) — restore the work branch before proceeding", branch, polecatName, beadID)
	}

	res, rErr := ensurePolecatWorkBranch(ctx.WorkDir, polecatName, beadID)
	if rErr != nil {
		printOffMainRefusal(branch, polecatName, beadID)
		fmt.Fprintf(os.Stderr, "Auto-restore failed: %v\n\n", rErr)
		return fmt.Errorf("polecat worktree on %q could not be auto-restored: %w", branch, rErr)
	}

	fmt.Fprintf(os.Stdout, "\n%s worktree was on %q; restored to %s before starting work.\n\n",
		style.Bold.Render("⚠️  OFF-MAIN GUARD:"), branch, res.Target)
	return nil
}

// printOffMainRefusal emits the loud block shown when a polecat is on main and
// the guard could not auto-restore it. Mirrors prime.go's "DATABASE ERROR"
// block style so the agent treats it as a hard stop, not advisory text.
func printOffMainRefusal(branch, polecatName, beadID string) {
	fmt.Fprintf(os.Stderr, "\n%s\n", style.Bold.Render("## 🚫 ON MAIN — DO NOT WORK OR RUN gt done 🚫"))
	fmt.Fprintf(os.Stderr, "Your polecat worktree HEAD is on %q. Polecats never commit on main —\n", branch)
	fmt.Fprintf(os.Stderr, "work goes through a polecat/<name>-<bead> branch and the merge queue.\n\n")
	fmt.Fprintf(os.Stderr, "Auto-restore could not run because the work branch name is unknown\n")
	fmt.Fprintf(os.Stderr, "(polecat=%q bead=%q).\n\n", polecatName, beadID)
	fmt.Fprintf(os.Stderr, "Recover before doing anything else:\n")
	if polecatName != "" && beadID != "" {
		fmt.Fprintf(os.Stderr, "  gt polecat checkout-branch %s\n\n", beadID)
	} else {
		fmt.Fprintf(os.Stderr, "  gt polecat checkout-branch <bead-id>   # the bead on your hook\n\n")
	}
	fmt.Fprintf(os.Stderr, "If that fails, escalate — do NOT commit on main:\n")
	fmt.Fprintf(os.Stderr, "  gt escalate -s HIGH \"polecat stuck on %s, cannot restore work branch\"\n\n", branch)
}
