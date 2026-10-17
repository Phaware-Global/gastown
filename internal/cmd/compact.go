package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/wisp"
)

var (
	compactDryRun  bool
	compactVerbose bool
	compactJSON    bool
	compactRig     string

	compactMaxPromotes int
	compactMaxDeletes  int
)

const (
	// defaultCompactMaxPromotes and defaultCompactMaxDeletes cap one run's
	// writes. Modest on purpose: the first run after a long blind period meets
	// a backlog of tens of thousands, and the deletes are permanent.
	defaultCompactMaxPromotes = 100
	defaultCompactMaxDeletes  = 500
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
	Skipped          int             `json:"skipped"`                     // wisps still within TTL, or exempt
	OrphanedOpen     int             `json:"orphaned_open,omitempty"`     // untyped open wisps with no sign of real work, left alone
	DeferredPromotes int             `json:"deferred_promotes,omitempty"` // eligible but over --max-promotes
	DeferredDeletes  int             `json:"deferred_deletes,omitempty"`  // eligible but over --max-deletes
	OrphanedWispDeps int             `json:"orphaned_wisp_deps"`          // stale wisp_dependencies removed
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
For closed wisps past TTL: deletes them. Wisps are not versioned by Dolt, so
deletes are permanent and cannot be recovered.
Wisps with comments or keep labels are always promoted.

Each run is capped (--max-promotes, --max-deletes), oldest wisps first, and
writes are batched and paced; a large backlog drains over successive runs.
Left alone regardless of age: open merge-request wisps and closed ones inside
the reaper's 24h recovery window, hooked or pinned wisps, open mail,
acknowledged escalations, agent beads, and steps of a live molecule. Open
untyped wisps with no parent (orphaned formula steps) are counted, not
promoted: promotion needs a wisp type, comments or a keep label. TTL overrides
below 1h are ignored.

TTLs by wisp type:
  heartbeat, ping:              6h
  patrol, gc_report:            24h
  recovery, error, escalation:  7d
  default (untyped):            24h

Examples:
  gt compact              # Run compaction
  gt compact --dry-run    # Preview what would happen
  gt compact --max-deletes 2000   # Drain a backlog faster
  gt compact --verbose    # Show each wisp decision
  gt compact --json       # Machine-readable output`,
	RunE: runCompact,
}

func init() {
	compactCmd.Flags().BoolVar(&compactDryRun, "dry-run", false, "Preview compaction without making changes")
	compactCmd.Flags().BoolVarP(&compactVerbose, "verbose", "v", false, "Show each wisp decision")
	compactCmd.Flags().BoolVar(&compactJSON, "json", false, "Output results as JSON")
	compactCmd.Flags().StringVar(&compactRig, "rig", "", "Compact a specific rig (default: current rig)")
	compactCmd.Flags().IntVar(&compactMaxPromotes, "max-promotes", defaultCompactMaxPromotes, "Promote at most this many wisps per run")
	compactCmd.Flags().IntVar(&compactMaxDeletes, "max-deletes", defaultCompactMaxDeletes, "Delete at most this many wisps per run (deletes are permanent)")

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
		applyTTLConfig(ttls, cfg.Get("wisp_ttl"))

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

	applyTTLLabels(ttls, issue.Labels)
}

// applyTTLConfig applies the rig config's wisp_ttl map (wisp type -> duration
// string) over ttls.
func applyTTLConfig(ttls map[string]time.Duration, raw interface{}) {
	// wisp_ttl is stored as map[string]interface{} in JSON config
	ttlMap, ok := raw.(map[string]interface{})
	if !ok {
		return
	}
	for wispType, val := range ttlMap {
		if s, ok := val.(string); ok {
			setTTL(ttls, wispType, s)
		}
	}
}

// applyTTLLabels applies wisp_ttl_<type>:<duration> labels over ttls.
func applyTTLLabels(ttls map[string]time.Duration, labels []string) {
	for _, label := range labels {
		colonIdx := strings.Index(label, ":")
		if colonIdx == -1 {
			continue
		}
		key := strings.ToLower(label[:colonIdx])
		value := strings.TrimSpace(label[colonIdx+1:])

		if wispType, ok := beads.ParseWispTTLKey(key); ok {
			setTTL(ttls, wispType, value)
		}
	}
}

// minWispTTL is the shortest TTL an override may set.
const minWispTTL = time.Hour

// setTTL parses value as a duration and stores it for wispType. A tiny TTL
// (0s, -1h, 1ns) would make every wisp of that type deletable at once, and the
// override can come from a label any agent may set, so anything under
// minWispTTL is ignored.
func setTTL(ttls map[string]time.Duration, wispType, value string) {
	if d, err := time.ParseDuration(value); err == nil && d >= minWispTTL {
		ttls[wispType] = d
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
	HasParent    bool   `json:"-"` // a molecule step
	ParentOpen   bool   `json:"-"` // parent molecule is still live
	IsMR         bool   `json:"-"` // gt:merge-request wisp
	MRPurgeable  bool   `json:"-"` // MR wisp past the reaper's recovery window (true for non-MR)
}

func runCompact(cmd *cobra.Command, args []string) error {
	now := time.Now().UTC()

	if compactMaxPromotes < 1 || compactMaxDeletes < 1 {
		return fmt.Errorf("--max-promotes and --max-deletes must be at least 1")
	}

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

	bd := beads.New(workDir)
	result, err := runCompactWith(bd.Run, ttls, now, compactLimits{
		MaxPromotes: compactMaxPromotes,
		MaxDeletes:  compactMaxDeletes,
	})
	if err != nil {
		return err
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

// compactLimits bounds how many writes a single run may issue. The wisps
// table can hold tens of thousands of expired rows; draining them in one pass
// would hammer the shared Dolt server, so the backlog drains over successive
// runs instead.
type compactLimits struct {
	MaxPromotes int
	MaxDeletes  int
}

const (
	// compactWriteBatch is how many ids one bd write covers.
	compactWriteBatch = 50
	// compactMaxConsecutiveFailures stops a run that keeps failing to write.
	compactMaxConsecutiveFailures = 5
)

// compactBatchPause spaces successive write batches. A var so tests can
// zero it.
var compactBatchPause = 250 * time.Millisecond

// errNoWispTables means the database has no wisp schema, so there is nothing
// to compact. Not a failure: the reaper skips such databases the same way.
var errNoWispTables = errors.New("database has no wisp tables")

// runCompactWith scans, plans and applies one compaction run through run, a
// `bd` subcommand runner.
func runCompactWith(run wispSQLRunner, ttls map[string]time.Duration, now time.Time, lim compactLimits) (*compactResult, error) {
	allWisps, err := listWispsWith(run)
	if errors.Is(err, errNoWispTables) {
		fmt.Fprintln(os.Stderr, "gt compact: no wisp tables in this database; nothing to compact")
		return &compactResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing wisps: %w", err)
	}

	if len(allWisps) == 0 {
		if warning := blindScanWarning(run); warning != "" {
			fmt.Fprintln(os.Stderr, warning)
		}
	}

	if !compactJSON && !compactDryRun {
		fmt.Printf("Compacting %d wisps...\n", len(allWisps))
	}

	plan := planWispCompaction(allWisps, ttls, now, lim)
	result := &compactResult{
		Skipped:          plan.Skipped,
		OrphanedOpen:     plan.OrphanedOpen,
		DeferredPromotes: plan.DeferredPromotes,
		DeferredDeletes:  plan.DeferredDeletes,
		Errors:           plan.Errors,
	}
	applyWispCompactPlan(run, plan, result)

	// Clean up orphaned wisp_dependencies left behind by deleted wisps.
	// When bd delete removes a wisp, it doesn't cascade-delete dependency
	// records in wisp_dependencies that reference the deleted wisp. Over many
	// compaction cycles these accumulate as dangling refs. We sweep them here.
	if !compactDryRun {
		cleanOrphanedWispDeps(run, result)
	}
	return result, nil
}

// wispCompactPlan is the set of writes one run intends to make, already capped.
type wispCompactPlan struct {
	Promote          []compactAction
	Delete           []compactAction
	Skipped          int
	OrphanedOpen     int // untyped open parentless wisps past TTL: not promoted
	DeferredPromotes int // eligible, but over the per-run cap
	DeferredDeletes  int
	Errors           []string
}

// compactExemption says why a wisp must be left alone however old it is, or
// "" when ordinary TTL rules apply.
func compactExemption(w *compactIssue) string {
	closed := w.Status == "closed"
	switch {
	case w.IsMR && !closed:
		// An open MR wisp is the only record tying a PR to the refinery queue.
		return "open merge-request wisp"
	case w.IsMR && !w.MRPurgeable:
		// Same recovery window the reaper keeps (gt-9yhyh).
		return "merge-request wisp inside its recovery window"
	case w.Status == "hooked" || w.Status == "pinned":
		return "live assignment (" + w.Status + ")"
	case !closed && hasLabel(w.Labels, "acked"):
		// An acknowledged escalation is finished with; its own lifecycle
		// closes it. Promoting it would resurrect it as an open issue.
		return "acknowledged escalation"
	case !closed && hasLabel(w.Labels, "gt:message"):
		// Mail is aged out by the reaper's mail purge; promoting it would
		// turn every unread message into a permanent bead.
		return "open mail"
	case w.HasParent && w.ParentOpen:
		// Steps of a live molecule are in use; leave them to their owner.
		return "step of a live molecule"
	}
	return ""
}

// planWispCompaction decides, oldest wisp first, what to promote and delete, and
// stops adding writes once a cap is reached.
func planWispCompaction(wisps []*compactIssue, ttls map[string]time.Duration, now time.Time, lim compactLimits) *wispCompactPlan {
	plan := &wispCompactPlan{}

	for _, w := range wisps {
		age, err := wispAge(w, now)
		if err != nil {
			plan.Errors = append(plan.Errors, fmt.Sprintf("%s: %v", w.ID, err))
			continue
		}

		if reason := compactExemption(w); reason != "" {
			plan.Skipped++
			if compactVerbose && !compactJSON {
				fmt.Printf("  skip  %s %s (%s)\n", w.ID, compactTruncate(w.Title, 40), reason)
			}
			continue
		}

		ttl := getTTL(ttls, w.WispType)
		shouldPromote := hasComments(w) || hasKeepLabel(w)

		// Molecule step wisps (those with a parent) should never be promoted.
		// They are subordinate steps of a molecule and should be deleted when
		// past TTL, not elevated to permanent beads. This prevents patrol
		// molecule steps from polluting the issues table.
		isMoleculeStep := w.HasParent
		action := compactAction{ID: w.ID, Title: w.Title, WispType: w.WispType}

		switch {
		case shouldPromote && !isMoleculeStep:
			action.Reason = "proven value"
			plan.addPromote(action, lim)
		case age <= ttl:
			plan.Skipped++
			if compactVerbose && !compactJSON {
				fmt.Printf("  skip  %s %s (age: %s, ttl: %s)\n",
					w.ID, compactTruncate(w.Title, 40), age.Round(time.Minute), ttl)
			}
		case w.Status == "closed":
			action.Reason = "TTL expired"
			plan.addDelete(action, lim)
		case isMoleculeStep:
			action.Reason = "molecule step past TTL"
			plan.addDelete(action, lim)
		case w.Status == "in_progress":
			// Someone claimed it: a sign of real work that stalled.
			action.Reason = "stuck in_progress past TTL"
			plan.addPromote(action, lim)
		case w.WispType == "":
			// Open, parentless and untyped is what an orphaned formula step
			// looks like (its molecule is gone). Age alone is no sign of real
			// work, and promoting thousands would flood the issues table with
			// open tasks. Promotion needs a type (error, escalation, ...),
			// comments or a keep label; the rest is counted and left alone.
			plan.OrphanedOpen++
		default:
			action.Reason = "open past TTL"
			plan.addPromote(action, lim)
		}
	}
	return plan
}

func (p *wispCompactPlan) addPromote(a compactAction, lim compactLimits) {
	if len(p.Promote) >= lim.MaxPromotes {
		p.DeferredPromotes++
		return
	}
	p.Promote = append(p.Promote, a)
}

func (p *wispCompactPlan) addDelete(a compactAction, lim compactLimits) {
	if len(p.Delete) >= lim.MaxDeletes {
		p.DeferredDeletes++
		return
	}
	p.Delete = append(p.Delete, a)
}

// applyWispCompactPlan performs the planned writes in paced batches. Under
// --dry-run it only reports them.
func applyWispCompactPlan(run wispSQLRunner, plan *wispCompactPlan, result *compactResult) {
	if compactDryRun {
		for _, a := range plan.Promote {
			result.Promoted = append(result.Promoted, a)
			printDryRunAction("promote", a)
		}
		for _, a := range plan.Delete {
			result.Deleted = append(result.Deleted, a)
			printDryRunAction("delete ", a)
		}
		return
	}

	w := &compactWriter{run: run, result: result}
	w.promote(plan.Promote)
	w.delete(plan.Delete)
}

func printDryRunAction(verb string, a compactAction) {
	if compactJSON {
		return
	}
	fmt.Printf("  %s %s %s %s (%s)\n",
		style.Dim.Render("[dry-run]"), verb, a.ID, compactTruncate(a.Title, 40), a.Reason)
}

// compactWriter issues batched bd writes and gives up after repeated
// failures, so a sick Dolt server is not hammered with the rest of the plan.
type compactWriter struct {
	run         wispSQLRunner
	result      *compactResult
	consecutive int  // failed writes in a row
	aborted     bool // too many failures; remaining writes are deferred
}

// batches runs fn over actions in compactWriteBatch-sized chunks with a pause
// between chunks, stopping once the writer has aborted.
func (w *compactWriter) batches(actions []compactAction, fn func(batch []compactAction)) (deferred int) {
	for start := 0; start < len(actions); start += compactWriteBatch {
		if w.aborted {
			return len(actions) - start
		}
		if start > 0 {
			time.Sleep(compactBatchPause)
		}
		end := start + compactWriteBatch
		if end > len(actions) {
			end = len(actions)
		}
		fn(actions[start:end])
	}
	return 0
}

// write runs one bd write. On failure it records the error and counts toward
// the abort threshold.
func (w *compactWriter) write(what string, args ...string) bool {
	if _, err := w.run(args...); err != nil {
		w.result.Errors = append(w.result.Errors, fmt.Sprintf("%s: %v", what, err))
		w.consecutive++
		if w.consecutive >= compactMaxConsecutiveFailures {
			w.aborted = true
			w.result.Errors = append(w.result.Errors,
				fmt.Sprintf("aborting after %d consecutive write failures; remaining work deferred", w.consecutive))
		}
		return false
	}
	w.consecutive = 0
	return true
}

// promote makes wisps permanent by setting --persistent and adding a comment.
func (w *compactWriter) promote(actions []compactAction) {
	w.result.DeferredPromotes += w.batches(actions, func(batch []compactAction) {
		ok := w.writeEach(batch, "update", "--persistent")
		for _, a := range ok {
			// Comment noting the promotion; best effort.
			_, _ = w.run("comments", "add", a.ID, fmt.Sprintf("Promoted from Level 0: %s", a.Reason))
			w.result.Promoted = append(w.result.Promoted, a)
			if compactVerbose && !compactJSON {
				fmt.Printf("  %s %s %s (%s)\n",
					style.Success.Render("promote"), a.ID, compactTruncate(a.Title, 40), a.Reason)
			}
		}
	})
}

// delete removes expired wisps. Permanent: wisps are dolt-ignored, so there is
// no AS OF history to recover a deleted row from.
func (w *compactWriter) delete(actions []compactAction) {
	w.result.DeferredDeletes += w.batches(actions, func(batch []compactAction) {
		ok := w.writeEach(batch, "delete", "--force")
		for _, a := range ok {
			w.result.Deleted = append(w.result.Deleted, a)
			if compactVerbose && !compactJSON {
				fmt.Printf("  %s %s %s (%s)\n",
					style.Warning.Render("delete "), a.ID, compactTruncate(a.Title, 40), a.Reason)
			}
		}
	})
}

// writeEach applies `bd <verb> <ids...> <flags...>` to a batch. If the batch
// write fails it retries the ids one at a time so a single bad id cannot
// block the rest. Returns the actions that were written.
func (w *compactWriter) writeEach(batch []compactAction, verb string, flags ...string) []compactAction {
	ids := make([]string, len(batch))
	for i, a := range batch {
		ids[i] = a.ID
	}
	if _, err := w.run(append(append([]string{verb}, ids...), flags...)...); err == nil {
		w.consecutive = 0
		return batch
	}
	var done []compactAction
	for _, a := range batch {
		if w.aborted {
			break
		}
		if w.write(fmt.Sprintf("%s %s", verb, a.ID), append([]string{verb, a.ID}, flags...)...) {
			done = append(done, a)
		}
	}
	return done
}

// cleanOrphanedWispDeps removes wisp_dependencies rows where either side no
// longer exists in the wisps table. This happens when bd delete removes a wisp
// but leaves behind its dependency records (bd delete has no cascade logic for
// the wisp-level tables). Runs as a post-compact sweep.
func cleanOrphanedWispDeps(run wispSQLRunner, result *compactResult) {
	columns, err := run("sql", "--csv", "SHOW COLUMNS FROM wisp_dependencies")
	if err != nil || !strings.Contains(string(columns), "\ndepends_on_wisp_id,") || !strings.Contains(string(columns), "\ndepends_on_issue_id,") {
		return
	}
	const q = `DELETE FROM wisp_dependencies WHERE ` +
		`NOT EXISTS (SELECT 1 FROM wisps WHERE id = wisp_dependencies.issue_id) ` +
		`OR (depends_on_wisp_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM wisps WHERE id = wisp_dependencies.depends_on_wisp_id)) ` +
		`OR (depends_on_issue_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM issues WHERE id = wisp_dependencies.depends_on_issue_id))`
	out, err := run("sql", q)
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

// wispScanFrom is the FROM/WHERE shared by the wisp scan and the blind-scan
// count, so both see the same population. Eligibility comes from the reaper's
// own SQL: agent identity beads are excluded, and parentage and merge-request
// recovery windows are read with the reaper's joins.
func wispScanFrom() (from, where string) {
	agentJoin, agentWhere := reaper.NotAgentWispJoin("w")
	return "FROM wisps w " + agentJoin, agentWhere
}

// blindScanWarning returns a warning when a scan of zero wisps coexists with a
// non-empty scan population, or "". Zero is normal only on an empty table;
// anything else means the scan is blind and no TTL is being enforced.
func blindScanWarning(run wispSQLRunner) string {
	from, where := wispScanFrom()
	var rows []struct {
		N int `json:"n"`
	}
	if err := wispQuery(run, &rows, "SELECT COUNT(*) AS n "+from+" WHERE "+where); err != nil || len(rows) == 0 || rows[0].N == 0 {
		return ""
	}
	return fmt.Sprintf("%s scanned 0 wisps but the wisps table holds %d non-agent rows; compaction is not enforcing TTL",
		style.Warning.Render("⚠"), rows[0].N)
}

// wispSQLRunner runs a bd subcommand and returns its stdout. *beads.Beads.Run
// satisfies it; tests substitute a fake.
type wispSQLRunner func(args ...string) ([]byte, error)

// requiredWispTables are the tables the scan reads.
var requiredWispTables = []string{"wisps", "wisp_labels", "wisp_comments", "wisp_dependencies", "labels", "issues"}

// hasWispSchema reports whether the database has the tables and columns the
// scan reads, as the reaper's HasReaperSchema does for its own queries. SHOW
// TABLES rather than information_schema, which is slow on a multi-database
// Dolt server.
func hasWispSchema(run wispSQLRunner) (bool, error) {
	out, err := run("sql", "--csv", "SHOW TABLES")
	if err != nil {
		return false, fmt.Errorf("checking wisp schema: %w", err)
	}
	have := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		have[strings.Trim(strings.TrimSpace(line), `"`)] = true
	}
	for _, table := range requiredWispTables {
		if !have[table] {
			return false, nil
		}
	}
	columns, err := run("sql", "--csv", "SHOW COLUMNS FROM wisp_dependencies")
	if err != nil {
		return false, fmt.Errorf("checking wisp_dependencies columns: %w", err)
	}
	for _, col := range []string{"depends_on_wisp_id", "depends_on_issue_id", "depends_on_external"} {
		if !strings.Contains(string(columns), "\n"+col+",") {
			return false, nil
		}
	}
	return true, nil
}

// listWisps reads every wisp from the wisps table.
//
// `bd list` cannot be used here: wisps live in their own table, so it never
// returns them and the old Ephemeral filter always matched zero rows. Reading
// the table directly is also how `gt reaper` sees wisps, so the two tools
// count the same population. Wisp-backed agent identity beads are excluded:
// they are standing records, never subject to TTL promotion or deletion.
// Returns extended issue structs with comment_count and wisp_type, oldest
// first. Returns errNoWispTables when the database has no wisp schema.
func listWisps(bd *beads.Beads) ([]*compactIssue, error) {
	return listWispsWith(bd.Run)
}

func listWispsWith(run wispSQLRunner) ([]*compactIssue, error) {
	ok, err := hasWispSchema(run)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNoWispTables
	}

	from, where := wispScanFrom()
	parentJoin, _ := reaper.ParentExcludeJoin("")
	mrJoin, mrWhere := reaper.PurgeableMRWispJoin("w")
	var rows []struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Status      string `json:"status"`
		IssueType   string `json:"issue_type"`
		WispType    string `json:"wisp_type"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
		HasParent   int    `json:"has_parent"`
		ParentOpen  int    `json:"parent_open"`
		IsMR        int    `json:"is_mr"`
		MRPurgeable int    `json:"mr_purgeable"`
	}
	query := "SELECT w.id AS id, w.title AS title, w.status AS status, w.issue_type AS issue_type, " +
		"COALESCE(w.wisp_type, '') AS wisp_type, w.created_at AS created_at, w.updated_at AS updated_at, " +
		"CASE WHEN has_parent.issue_id IS NOT NULL THEN 1 ELSE 0 END AS has_parent, " +
		"CASE WHEN open_parent.issue_id IS NOT NULL THEN 1 ELSE 0 END AS parent_open, " +
		"CASE WHEN mr_wl.issue_id IS NOT NULL THEN 1 ELSE 0 END AS is_mr, " +
		"CASE WHEN " + mrWhere + " THEN 1 ELSE 0 END AS mr_purgeable " +
		from + " " + parentJoin + " " + mrJoin + " WHERE " + where + " ORDER BY w.updated_at, w.id"
	if err := wispQuery(run, &rows, query); err != nil {
		return nil, fmt.Errorf("querying wisps: %w", err)
	}

	var labelRows []struct {
		IssueID string `json:"issue_id"`
		Label   string `json:"label"`
	}
	if err := wispQuery(run, &labelRows, "SELECT issue_id, label FROM wisp_labels"); err != nil {
		return nil, fmt.Errorf("querying wisp_labels: %w", err)
	}

	var commentRows []struct {
		IssueID string `json:"issue_id"`
		N       int    `json:"n"`
	}
	if err := wispQuery(run, &commentRows,
		"SELECT issue_id, COUNT(*) AS n FROM wisp_comments GROUP BY issue_id"); err != nil {
		return nil, fmt.Errorf("querying wisp_comments: %w", err)
	}

	labels := make(map[string][]string)
	for _, r := range labelRows {
		labels[r.IssueID] = append(labels[r.IssueID], r.Label)
	}
	comments := make(map[string]int)
	for _, r := range commentRows {
		comments[r.IssueID] = r.N
	}

	var wisps []*compactIssue
	for _, r := range rows {
		wisps = append(wisps, &compactIssue{
			Issue: beads.Issue{
				ID:        r.ID,
				Title:     r.Title,
				Status:    r.Status,
				Type:      r.IssueType,
				CreatedAt: r.CreatedAt,
				UpdatedAt: r.UpdatedAt,
				Labels:    labels[r.ID],
				Ephemeral: true,
			},
			CommentCount: comments[r.ID],
			WispType:     r.WispType,
			HasParent:    r.HasParent != 0,
			ParentOpen:   r.ParentOpen != 0,
			IsMR:         r.IsMR != 0,
			MRPurgeable:  r.MRPurgeable != 0,
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
func extractJSONArray(data []byte) []byte {
	idx := bytes.IndexByte(data, '[')
	if idx < 0 {
		return data
	}
	return data[idx:]
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
	fmt.Printf("  Skipped:  %d (within TTL or exempt)\n", result.Skipped)
	if result.DeferredPromotes > 0 || result.DeferredDeletes > 0 {
		fmt.Printf("  Deferred: %d promote(s), %d delete(s) over the per-run cap (re-run, or raise --max-promotes / --max-deletes)\n",
			result.DeferredPromotes, result.DeferredDeletes)
	}
	if result.OrphanedOpen > 0 {
		fmt.Printf("  Orphaned: %d open untyped wisp(s) left alone (not promoted)\n", result.OrphanedOpen)
	}
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
