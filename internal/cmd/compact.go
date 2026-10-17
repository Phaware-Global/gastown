package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/wisp"
)

var (
	compactDryRun  bool
	compactVerbose bool
	compactJSON    bool
	compactRig     string
)

// Default TTLs per wisp type (from design doc WISP-COMPACTION-POLICY.md).
var defaultTTLs = map[string]time.Duration{
	"heartbeat":  6 * time.Hour,
	"ping":       6 * time.Hour,
	"patrol":     24 * time.Hour,
	"gc_report":  24 * time.Hour,
	"recovery":   7 * 24 * time.Hour,
	"error":      7 * 24 * time.Hour,
	"escalation": 7 * 24 * time.Hour,
	"default":    24 * time.Hour,
}

// compactResult tracks what happened to each wisp during compaction.
type compactResult struct {
	Promoted         []compactAction `json:"promoted"`
	Deleted          []compactAction `json:"deleted"`
	Skipped          int             `json:"skipped"`            // wisps still within TTL
	OrphanedWispDeps int             `json:"orphaned_wisp_deps"` // stale wisp_dependencies removed
	Errors           []string        `json:"errors,omitempty"`
}

type compactAction struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Reason   string `json:"reason"`
	WispType string `json:"wisp_type,omitempty"`
}

var compactCmd = &cobra.Command{
	Use:     "compact",
	GroupID: GroupWork,
	Short:   "Compact expired wisps (TTL-based cleanup)",
	Long: `Apply TTL-based compaction policy to ephemeral wisps.

For non-closed wisps past TTL: promotes to permanent beads (something is stuck).
For closed wisps past TTL: deletes them (Dolt AS OF preserves history).
Wisps with comments or keep labels are always promoted.

TTLs by wisp type:
  heartbeat, ping:              6h
  patrol, gc_report:            24h
  recovery, error, escalation:  7d
  default (untyped):            24h

Examples:
  gt compact              # Run compaction
  gt compact --dry-run    # Preview what would happen
  gt compact --verbose    # Show each wisp decision
  gt compact --json       # Machine-readable output`,
	RunE: runCompact,
}

func init() {
	compactCmd.Flags().BoolVar(&compactDryRun, "dry-run", false, "Preview compaction without making changes")
	compactCmd.Flags().BoolVarP(&compactVerbose, "verbose", "v", false, "Show each wisp decision")
	compactCmd.Flags().BoolVar(&compactJSON, "json", false, "Output results as JSON")
	compactCmd.Flags().StringVar(&compactRig, "rig", "", "Compact a specific rig (default: current rig)")

	rootCmd.AddCommand(compactCmd)
}

// loadTTLConfig loads TTL configuration with layered precedence:
//
//	rig config (wisp layer + bead labels) > hardcoded defaults
func loadTTLConfig(townRoot, rigName string) map[string]time.Duration {
	return loadTTLConfigWithRole(townRoot, rigName)
}

// loadTTLConfigWithRole is the testable version of loadTTLConfig.
func loadTTLConfigWithRole(townRoot, rigName string) map[string]time.Duration {
	// Layer 1: Hardcoded defaults (lowest precedence)
	ttls := make(map[string]time.Duration)
	for k, v := range defaultTTLs {
		ttls[k] = v
	}

	if townRoot == "" {
		return ttls
	}

	// Layer 2: Rig config - wisp layer (middle precedence)
	if rigName != "" {
		cfg := wisp.NewConfig(townRoot, rigName)
		raw := cfg.Get("wisp_ttl")
		if raw != nil {
			// wisp_ttl is stored as map[string]interface{} in JSON config
			if ttlMap, ok := raw.(map[string]interface{}); ok {
				for wispType, val := range ttlMap {
					if s, ok := val.(string); ok {
						if d, err := time.ParseDuration(s); err == nil {
							ttls[wispType] = d
						}
					}
				}
			}
		}

		// Layer 2b: Rig identity bead labels (wisp_ttl_*:value)
		applyRigBeadTTLOverrides(ttls, townRoot, rigName)
	}

	return ttls
}

// applyRigBeadTTLOverrides reads wisp_ttl_* labels from the rig identity bead
// and applies them as overrides.
func applyRigBeadTTLOverrides(ttls map[string]time.Duration, townRoot, rigName string) {
	beadsDir := beads.ResolveBeadsDir(townRoot)
	bd := beads.NewWithBeadsDir(townRoot, beadsDir)

	rigBeadID := beads.RigBeadIDWithPrefix("gt", rigName)
	issue, err := bd.Show(rigBeadID)
	if err != nil {
		return
	}

	for _, label := range issue.Labels {
		colonIdx := strings.Index(label, ":")
		if colonIdx == -1 {
			continue
		}
		key := strings.ToLower(label[:colonIdx])
		value := strings.TrimSpace(label[colonIdx+1:])

		if wispType, ok := beads.ParseWispTTLKey(key); ok {
			if dur, err := time.ParseDuration(value); err == nil {
				ttls[wispType] = dur
			}
		}
	}
}

// getTTL returns the TTL for a wisp based on its wisp_type field.
// Falls back to "default" if wisp_type is empty or unknown.
func getTTL(ttls map[string]time.Duration, wispType string) time.Duration {
	if wispType == "" {
		wispType = "default"
	}
	if d, ok := ttls[wispType]; ok {
		return d
	}
	return ttls["default"]
}

// compactIssue is the extended issue struct that includes fields from bd list --json.
type compactIssue struct {
	beads.Issue
	CommentCount int    `json:"comment_count"`
	WispType     string `json:"wisp_type,omitempty"`
	ParentOpen   bool   `json:"-"` // parent molecule is still live
}

func runCompact(cmd *cobra.Command, args []string) error {
	now := time.Now().UTC()

	// Resolve working directory and town root
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working dir: %w", err)
	}

	townRoot := beads.FindTownRoot(workDir)
	rigName := compactRig
	if rigName == "" {
		rigName = os.Getenv("GT_RIG")
	}

	// Load TTL config
	ttls := loadTTLConfig(townRoot, rigName)

	// Query all ephemeral (wisp) issues via bd list
	bd := beads.New(workDir)
	allWisps, err := listWisps(bd)
	if err != nil {
		return fmt.Errorf("listing wisps: %w", err)
	}

	if len(allWisps) == 0 {
		warnIfBlindScan(bd)
	}

	if !compactJSON && !compactDryRun {
		fmt.Printf("Compacting %d wisps...\n", len(allWisps))
	}

	result := &compactResult{}

	for _, w := range allWisps {
		age, err := wispAge(w, now)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", w.ID, err))
			continue
		}

		ttl := getTTL(ttls, w.WispType)
		shouldPromote := hasComments(w) || hasKeepLabel(w)

		// Molecule step wisps (those with a Parent) should never be promoted.
		// They are subordinate steps of a molecule and should be deleted when
		// past TTL, not elevated to permanent beads. This prevents patrol
		// molecule steps from polluting the issues table.
		isMoleculeStep := w.Parent != ""

		// Steps of a live molecule are in use; leave them to their owner.
		if isMoleculeStep && w.ParentOpen {
			result.Skipped++
			continue
		}

		if w.Status != "closed" {
			// Non-closed wisps
			if shouldPromote && !isMoleculeStep {
				promoteWisp(bd, w, "proven value", result)
			} else if age > ttl {
				if isMoleculeStep {
					deleteWisp(bd, w, "molecule step past TTL", result)
				} else {
					reason := "open past TTL"
					if w.Status == "in_progress" {
						reason = "stuck in_progress past TTL"
					}
					promoteWisp(bd, w, reason, result)
				}
			} else {
				result.Skipped++
				if compactVerbose && !compactJSON {
					fmt.Printf("  skip  %s %s (age: %s, ttl: %s)\n",
						w.ID, compactTruncate(w.Title, 40), age.Round(time.Minute), ttl)
				}
			}
		} else {
			// Closed wisps
			if shouldPromote && !isMoleculeStep {
				promoteWisp(bd, w, "proven value", result)
			} else if age > ttl {
				deleteWisp(bd, w, "TTL expired", result)
			} else {
				result.Skipped++
				if compactVerbose && !compactJSON {
					fmt.Printf("  skip  %s %s (age: %s, ttl: %s)\n",
						w.ID, compactTruncate(w.Title, 40), age.Round(time.Minute), ttl)
				}
			}
		}
	}

	// Clean up orphaned wisp_dependencies left behind by deleted wisps.
	// When bd delete removes a wisp, it doesn't cascade-delete dependency
	// records in wisp_dependencies that reference the deleted wisp. Over many
	// compaction cycles these accumulate as dangling refs. We sweep them here.
	if !compactDryRun {
		cleanOrphanedWispDeps(bd, result)
	}

	// Output results
	if compactJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	printCompactSummary(result)
	return nil
}

// cleanOrphanedWispDeps removes wisp_dependencies rows where either side no
// longer exists in the wisps table. This happens when bd delete removes a wisp
// but leaves behind its dependency records (bd delete has no cascade logic for
// the wisp-level tables). Runs as a post-compact sweep.
func cleanOrphanedWispDeps(bd *beads.Beads, result *compactResult) {
	columns, err := bd.Run("sql", "--csv", "SHOW COLUMNS FROM wisp_dependencies")
	if err != nil || !strings.Contains(string(columns), "\ndepends_on_wisp_id,") || !strings.Contains(string(columns), "\ndepends_on_issue_id,") {
		return
	}
	const q = `DELETE FROM wisp_dependencies WHERE ` +
		`NOT EXISTS (SELECT 1 FROM wisps WHERE id = wisp_dependencies.issue_id) ` +
		`OR (depends_on_wisp_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM wisps WHERE id = wisp_dependencies.depends_on_wisp_id)) ` +
		`OR (depends_on_issue_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM issues WHERE id = wisp_dependencies.depends_on_issue_id))`
	out, err := bd.Run("sql", q)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("orphaned wisp_deps cleanup: %v", err))
		return
	}
	// bd sql reports "OK, N rows affected" for non-SELECT statements.
	// Parse the count if present; a non-zero result means refs were cleaned.
	var n int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(out)), "OK, %d rows affected", &n); scanErr == nil {
		result.OrphanedWispDeps = n
	}
}

// warnIfBlindScan complains when a scan of zero wisps coexists with a
// non-empty wisps table. Zero is normal only on an empty table; anything else
// means the scan is blind and no TTL is being enforced.
func warnIfBlindScan(bd *beads.Beads) {
	var rows []struct {
		N int `json:"n"`
	}
	if err := wispQuery(bd.Run, &rows, "SELECT COUNT(*) AS n FROM wisps"); err != nil || len(rows) == 0 || rows[0].N == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "%s scanned 0 wisps but the wisps table holds %d rows (agent beads excluded); compaction is not enforcing TTL\n",
		style.Warning.Render("⚠"), rows[0].N)
}

// wispSQLRunner runs a bd subcommand and returns its stdout. *beads.Beads.Run
// satisfies it; tests substitute a fake.
type wispSQLRunner func(args ...string) ([]byte, error)

// listWisps reads every wisp from the wisps table.
//
// `bd list` cannot be used here: wisps live in their own table, so it never
// returns them and the old Ephemeral filter always matched zero rows. Reading
// the table directly is also how `gt reaper` sees wisps, so the two tools
// count the same population. Wisp-backed agent identity beads are excluded:
// they are standing records, never subject to TTL promotion or deletion.
// Returns extended issue structs with comment_count and wisp_type.
func listWisps(bd *beads.Beads) ([]*compactIssue, error) {
	return listWispsWith(bd.Run)
}

func listWispsWith(run wispSQLRunner) ([]*compactIssue, error) {
	var rows []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		IssueType string `json:"issue_type"`
		WispType  string `json:"wisp_type"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := wispQuery(run, &rows,
		"SELECT id, title, status, issue_type, COALESCE(wisp_type, '') AS wisp_type, created_at, updated_at FROM wisps"); err != nil {
		return nil, fmt.Errorf("querying wisps: %w", err)
	}

	var labelRows []struct {
		IssueID string `json:"issue_id"`
		Label   string `json:"label"`
	}
	if err := wispQuery(run, &labelRows, "SELECT issue_id, label FROM wisp_labels"); err != nil {
		return nil, fmt.Errorf("querying wisp_labels: %w", err)
	}

	// bd create --labels writes agent markers to the labels table, not
	// wisp_labels, so the agent guard must consult both (see reaper's
	// notAgentWispJoin).
	var agentRows []struct {
		IssueID string `json:"issue_id"`
	}
	if err := wispQuery(run, &agentRows,
		"SELECT issue_id FROM labels WHERE label = 'gt:agent' AND issue_id IN (SELECT id FROM wisps)"); err != nil {
		return nil, fmt.Errorf("querying agent labels: %w", err)
	}

	var commentRows []struct {
		IssueID string `json:"issue_id"`
		N       int    `json:"n"`
	}
	if err := wispQuery(run, &commentRows,
		"SELECT issue_id, COUNT(*) AS n FROM wisp_comments GROUP BY issue_id"); err != nil {
		return nil, fmt.Errorf("querying wisp_comments: %w", err)
	}

	// A step with an open parent belongs to a live molecule; external parents
	// count as open because their state cannot be checked (reaper parity).
	var parentRows []struct {
		IssueID    string `json:"issue_id"`
		ParentID   string `json:"parent_id"`
		ParentOpen int    `json:"parent_open"`
	}
	if err := wispQuery(run, &parentRows,
		"SELECT wd.issue_id AS issue_id, "+
			"COALESCE(wd.depends_on_wisp_id, wd.depends_on_issue_id, wd.depends_on_external, '') AS parent_id, "+
			"CASE WHEN pw.status IN ('open', 'hooked', 'in_progress') OR pi.status IN ('open', 'hooked', 'in_progress') "+
			"OR wd.depends_on_external IS NOT NULL THEN 1 ELSE 0 END AS parent_open "+
			"FROM wisp_dependencies wd "+
			"LEFT JOIN wisps pw ON pw.id = wd.depends_on_wisp_id "+
			"LEFT JOIN issues pi ON pi.id = wd.depends_on_issue_id "+
			"WHERE wd.type = 'parent-child'"); err != nil {
		return nil, fmt.Errorf("querying wisp parents: %w", err)
	}

	labels := make(map[string][]string)
	for _, r := range labelRows {
		labels[r.IssueID] = append(labels[r.IssueID], r.Label)
	}
	isAgent := make(map[string]bool)
	for _, r := range agentRows {
		isAgent[r.IssueID] = true
	}
	comments := make(map[string]int)
	for _, r := range commentRows {
		comments[r.IssueID] = r.N
	}
	type parentInfo struct {
		id   string
		open bool
	}
	parents := make(map[string]parentInfo)
	for _, r := range parentRows {
		p := parents[r.IssueID]
		p.id = r.ParentID
		p.open = p.open || r.ParentOpen != 0
		parents[r.IssueID] = p
	}

	var wisps []*compactIssue
	for _, r := range rows {
		if r.IssueType == "agent" || isAgent[r.ID] {
			continue
		}
		for _, l := range labels[r.ID] {
			if l == "gt:agent" {
				isAgent[r.ID] = true
			}
		}
		if isAgent[r.ID] {
			continue
		}
		p := parents[r.ID]
		wisps = append(wisps, &compactIssue{
			Issue: beads.Issue{
				ID:        r.ID,
				Title:     r.Title,
				Status:    r.Status,
				Type:      r.IssueType,
				CreatedAt: r.CreatedAt,
				UpdatedAt: r.UpdatedAt,
				Parent:    p.id,
				Labels:    labels[r.ID],
				Ephemeral: true,
			},
			CommentCount: comments[r.ID],
			WispType:     r.WispType,
			ParentOpen:   p.open,
		})
	}
	return wisps, nil
}

// wispQuery runs a SELECT through `bd sql --json` and decodes the row array.
func wispQuery(run wispSQLRunner, dst interface{}, query string) error {
	out, err := run("sql", "--json", query)
	if err != nil {
		return err
	}
	// Strip any non-JSON prefix (warnings, notices) bd may emit to stdout.
	out = extractJSONArray(out)
	if err := json.Unmarshal(out, dst); err != nil {
		return fmt.Errorf("parsing sql output: %w", err)
	}
	return nil
}

// extractJSONArray finds the first '[' byte in data and returns from that
// point onward. This strips any non-JSON prefix (warning messages, notices)
// that a subprocess may emit to stdout before the actual JSON payload.
// Returns the original data unchanged if no '[' is found.
func extractJSONArray(data []byte) []byte {
	idx := bytes.IndexByte(data, '[')
	if idx < 0 {
		return data
	}
	return data[idx:]
}

// promoteWisp makes a wisp permanent by setting --persistent and adding a comment.
func promoteWisp(bd *beads.Beads, w *compactIssue, reason string, result *compactResult) {
	action := compactAction{ID: w.ID, Title: w.Title, Reason: reason, WispType: w.WispType}

	if compactDryRun {
		result.Promoted = append(result.Promoted, action)
		if !compactJSON {
			fmt.Printf("  %s promote %s %s (%s)\n",
				style.Dim.Render("[dry-run]"), w.ID, compactTruncate(w.Title, 40), reason)
		}
		return
	}

	// bd update --persistent sets ephemeral=false
	_, err := bd.Run("update", w.ID, "--persistent")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("promote %s: %v", w.ID, err))
		return
	}

	// Add comment noting the promotion
	_, _ = bd.Run("comments", "add", w.ID, fmt.Sprintf("Promoted from Level 0: %s", reason))

	result.Promoted = append(result.Promoted, action)

	if compactVerbose && !compactJSON {
		fmt.Printf("  %s %s %s (%s)\n",
			style.Success.Render("promote"), w.ID, compactTruncate(w.Title, 40), reason)
	}
}

// deleteWisp removes a closed wisp that has expired past its TTL.
func deleteWisp(bd *beads.Beads, w *compactIssue, reason string, result *compactResult) {
	action := compactAction{ID: w.ID, Title: w.Title, Reason: reason, WispType: w.WispType}

	if compactDryRun {
		result.Deleted = append(result.Deleted, action)
		if !compactJSON {
			fmt.Printf("  %s delete  %s %s (%s)\n",
				style.Dim.Render("[dry-run]"), w.ID, compactTruncate(w.Title, 40), reason)
		}
		return
	}

	// bd delete --force (safe: Dolt AS OF preserves history)
	_, err := bd.Run("delete", w.ID, "--force")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("delete %s: %v", w.ID, err))
		return
	}

	result.Deleted = append(result.Deleted, action)

	if compactVerbose && !compactJSON {
		fmt.Printf("  %s %s %s (%s)\n",
			style.Warning.Render("delete "), w.ID, compactTruncate(w.Title, 40), reason)
	}
}

func printCompactSummary(result *compactResult) {
	promoted := len(result.Promoted)
	deleted := len(result.Deleted)
	total := promoted + deleted + result.Skipped

	if compactDryRun {
		fmt.Printf("\n%s Dry run complete: %d wisps scanned\n",
			style.Dim.Render("ℹ"), total)
	} else {
		fmt.Printf("\n%s Compaction complete\n", style.Success.Render("✓"))
	}

	fmt.Printf("  Promoted: %d\n", promoted)
	fmt.Printf("  Deleted:  %d\n", deleted)
	fmt.Printf("  Skipped:  %d (within TTL)\n", result.Skipped)
	if result.OrphanedWispDeps > 0 {
		fmt.Printf("  Cleaned:  %d orphaned wisp dependency ref(s)\n", result.OrphanedWispDeps)
	}

	if len(result.Errors) > 0 {
		fmt.Printf("\n%s %d errors:\n", style.Warning.Render("⚠"), len(result.Errors))
		for _, e := range result.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}

	// Show promotions if any
	if promoted > 0 && !compactDryRun {
		fmt.Printf("\nPromotions:\n")
		for _, p := range result.Promoted {
			fmt.Printf("  %s: %s (%s)\n", p.ID, compactTruncate(p.Title, 50), p.Reason)
		}
	}
}

// compactTruncate shortens a string to maxLen runes, adding "..." if truncated.
// Uses rune count instead of byte length so multi-byte UTF-8 characters
// (emoji, CJK, etc.) are never split mid-sequence.
func compactTruncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string([]rune(s)[:maxLen])
	}
	return string([]rune(s)[:maxLen-3]) + "..."
}

// hasComments checks the comment_count on the compactIssue.
func hasComments(w *compactIssue) bool {
	return w.CommentCount > 0
}

// isReferenced checks dependency counts.
func isReferenced(w *compactIssue) bool {
	return w.DependentCount > 0 || w.DependencyCount > 0
}

// hasKeepLabel checks for keep labels.
func hasKeepLabel(w *compactIssue) bool {
	for _, label := range w.Labels {
		if label == "keep" || label == "gt:keep" {
			return true
		}
	}
	return false
}

// wispAge returns the age of a compactIssue.
func wispAge(w *compactIssue, now time.Time) (time.Duration, error) {
	ts := w.UpdatedAt
	if ts == "" {
		ts = w.CreatedAt
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			return 0, fmt.Errorf("parsing timestamp %q: %w", ts, err)
		}
	}
	return now.Sub(t), nil
}
