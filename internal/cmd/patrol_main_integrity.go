package cmd

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
)

// prSquashPattern matches the standard squash-merge commit subject produced by
// `gh pr merge --squash`: any title ending with " (#N)" where N is a decimal
// PR number. All authorised merges in a merge_strategy=pr rig land this way;
// anything that doesn't match is a candidate for unauthorized-push detection.
//
// Intentionally strict: we only accept "(#<digits>)" at the very end of the
// subject. A subject like "add thing (#12) and also #3" does NOT match — the
// PR number must be the final token, which is what gh squash merge produces.
var prSquashPattern = regexp.MustCompile(`\(#\d+\)$`)

// MainIntegrityFinding describes a single commit on main that does not look
// like an authorised squash-merge from the PR workflow.
type MainIntegrityFinding struct {
	SHA     string
	Subject string
	Author  string
	When    time.Time
}

// MainIntegrityResult holds the output of detectUnauthorizedMainPushes.
type MainIntegrityResult struct {
	// CheckedSince is the look-back window used for this scan.
	CheckedSince time.Time
	// Checked is the total number of commits examined on origin/main.
	Checked int
	// Unauthorized lists every commit whose subject doesn't match the
	// expected squash-merge pattern.
	Unauthorized []MainIntegrityFinding
	// Error is non-nil when the git operations themselves failed (e.g.
	// origin/main is unreachable). Callers should surface this to the
	// operator — a failed check is not the same as "all clear."
	Error error
}

// detectUnauthorizedMainPushes fetches origin/main and inspects the last
// `since` window of commits for subjects that don't match the expected PR
// squash-merge pattern.
//
// Only meaningful for rigs with merge_strategy=pr. Callers should gate on
// rigIsPRMode before calling, or treat non-PR rigs as always returning nil.
//
// workDir is the root of the git repository (typically townRoot or rigPath).
func detectUnauthorizedMainPushes(workDir string, since time.Duration) *MainIntegrityResult {
	result := &MainIntegrityResult{
		CheckedSince: time.Now().Add(-since),
	}

	// Fetch to get the latest state of origin/main.
	fetchCmd := exec.Command("git", "-C", workDir, "fetch", "origin", "main", "--quiet")
	if err := fetchCmd.Run(); err != nil {
		result.Error = fmt.Errorf("fetch origin/main: %w", err)
		return result
	}

	// List commits since the look-back window.
	// Format: SHA\x01subject\x01author-email, one commit per line.
	// We use \x01 (ASCII SOH) as a field separator — unlikely in commit subjects.
	sinceArg := fmt.Sprintf("--since=%s", since.String())
	logOut, err := exec.Command("git", "-C", workDir, "log", "origin/main",
		sinceArg,
		"--format=%H\x01%s\x01%ae",
	).Output()
	if err != nil {
		result.Error = fmt.Errorf("git log origin/main: %w", err)
		return result
	}

	lines := strings.Split(strings.TrimRight(string(logOut), "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x01", 3)
		if len(parts) != 3 {
			continue
		}
		sha, subject, author := parts[0], parts[1], parts[2]
		result.Checked++

		if isAuthorizedMainCommit(subject) {
			continue
		}
		result.Unauthorized = append(result.Unauthorized, MainIntegrityFinding{
			SHA:     sha,
			Subject: subject,
			Author:  author,
			When:    time.Now(), // approximate — not parsing git timestamp for simplicity
		})
	}

	return result
}

// isAuthorizedMainCommit returns true when a commit subject looks like an
// authorised merge into main under the PR workflow. It explicitly permits:
//
//   - Standard squash merges: subjects ending with " (#N)".
//   - Emergency reverts: subjects starting with `Revert "`. These bypass the
//     PR flow by design (the Mayor or maintainer reverts immediately to unblock).
//   - Upstream merges: subjects starting with "Merge " (e.g. upstream syncs).
//   - Initial/setup commits: the very first commit on a new branch typically
//     doesn't have a PR number — allow any subject containing "Initial" or
//     "initial commit".
//
// Everything else is treated as unauthorized and flagged for review.
func isAuthorizedMainCommit(subject string) bool {
	if prSquashPattern.MatchString(subject) {
		return true
	}
	// Revert and merge commits by maintainers/bots are allowed.
	if strings.HasPrefix(subject, "Revert \"") {
		return true
	}
	if strings.HasPrefix(subject, "Merge ") {
		return true
	}
	// Initial / bootstrap commits.
	lower := strings.ToLower(subject)
	if strings.Contains(lower, "initial commit") || subject == "Initial" {
		return true
	}
	return false
}

// sendMainIntegrityAlert mails the witness and mayor when unauthorized commits
// are detected on origin/main. Called from runPatrolScan when --notify is set.
func sendMainIntegrityAlert(router *mail.Router, rigName string, result *MainIntegrityResult) {
	var lines []string
	lines = append(lines, fmt.Sprintf("Main-integrity scan detected %d unauthorized commit(s) on %s/origin/main:", len(result.Unauthorized), rigName))
	lines = append(lines, "")
	lines = append(lines, "These commits did not go through the PR workflow (no `(#N)` suffix):")
	lines = append(lines, "")
	for _, f := range result.Unauthorized {
		lines = append(lines, fmt.Sprintf("  %s  %s  <%s>", f.SHA[:8], f.Subject, f.Author))
	}
	lines = append(lines, "")
	lines = append(lines, "Possible causes:")
	lines = append(lines, "  - A polecat auto-saved and pushed directly (gt-pvx incident class)")
	lines = append(lines, "  - A worktree was on main when gt done ran")
	lines = append(lines, "  - A refinery session improvised a direct push")
	lines = append(lines, "")
	lines = append(lines, "Action:")
	lines = append(lines, "  Review the commits above. If unauthorized, revert immediately:")
	lines = append(lines, "  git revert <SHA> && git push origin main")

	body := strings.Join(lines, "\n")
	subject := fmt.Sprintf("MAIN_INTEGRITY: %d unauthorized commit(s) on %s", len(result.Unauthorized), rigName)

	witMsg := &mail.Message{
		From:    fmt.Sprintf("%s/witness", rigName),
		To:      fmt.Sprintf("%s/witness", rigName),
		Subject: subject,
		Body:    body,
	}
	_ = router.Send(witMsg)

	mayorMsg := &mail.Message{
		From:    fmt.Sprintf("%s/witness", rigName),
		To:      "mayor/",
		Subject: subject,
		Body:    body,
	}
	_ = router.Send(mayorMsg)
}

// rigIsPRMode returns true when the rig at rigPath is configured with
// merge_strategy=pr. Used to gate main-integrity checks — the check is only
// meaningful on PR-mode rigs where every merge to main must arrive via a PR.
func rigIsPRMode(townRoot, rigName string) bool {
	rigPath := filepath.Join(townRoot, rigName)
	settingsPath := config.RigSettingsPath(rigPath)
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil || settings == nil || settings.MergeQueue == nil {
		return false
	}
	return settings.MergeQueue.MergeStrategy == config.MergeStrategyPR
}

// outputMainIntegrityHuman prints the main-integrity scan results to stdout.
func outputMainIntegrityHuman(result *MainIntegrityResult) {
	if result == nil {
		return
	}

	if result.Error != nil {
		fmt.Printf("%s Main Integrity: fetch failed — %v\n\n", style.Warning.Render("⚠"), result.Error)
		return
	}

	fmt.Printf("%s Main Integrity: checked %d commit(s) since %s\n",
		style.Bold.Render("🔒"), result.Checked,
		result.CheckedSince.UTC().Format("2006-01-02T15:04Z"))

	if len(result.Unauthorized) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("All commits via PR workflow — no unauthorized pushes"))
	} else {
		for _, f := range result.Unauthorized {
			fmt.Printf("  %s %s  %s  <%s>\n",
				style.Warning.Render("⚠"), f.SHA[:8], f.Subject, f.Author)
		}
	}
	fmt.Println()
}
