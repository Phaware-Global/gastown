package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

// RunResult represents the outcome of a plugin execution.
type RunResult string

const (
	ResultSuccess RunResult = "success"
	ResultFailure RunResult = "failure"
	ResultSkipped RunResult = "skipped"
)

// PluginRunRecord represents data for creating a plugin run bead.
type PluginRunRecord struct {
	PluginName string
	RigName    string
	Result     RunResult
	Body       string
}

// PluginRunBead represents a recorded plugin run from the ledger.
type PluginRunBead struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	Labels    []string  `json:"labels"`
	Result    RunResult `json:"-"` // Parsed from labels
}

// Recorder handles plugin run recording and querying.
//
// When store is non-nil, RecordRun and queryRuns use the in-process
// beadsdk.Storage directly instead of shelling out to the bd CLI. This
// eliminates an OS process spawn + MySQL session preamble per call. The
// daemon's plugin dispatch loop drives the bulk of recorder traffic, so
// attaching a store there is the high-leverage hookup. CLI callers that
// construct a Recorder without a store retain the bd-shell-out fallback.
type Recorder struct {
	townRoot string
	store    beadsdk.Storage
}

// NewRecorder creates a new plugin run recorder that shells out to bd.
func NewRecorder(townRoot string) *Recorder {
	return &Recorder{townRoot: townRoot}
}

// NewRecorderWithStore creates a recorder bound to an in-process beadsdk
// store. Used by the daemon to bypass bd subprocess spawning.
func NewRecorderWithStore(townRoot string, store beadsdk.Storage) *Recorder {
	return &Recorder{townRoot: townRoot, store: store}
}

// SetStore attaches an in-process store to an existing recorder. Subsequent
// calls bypass the bd subprocess.
func (r *Recorder) SetStore(store beadsdk.Storage) {
	r.store = store
}

// recorderCtx returns a context with a standard timeout.
func recorderCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), constants.BdCommandTimeout)
}

// RecordRun creates an ephemeral bead for a plugin run.
// This is pure data writing - the caller decides what result to record.
func (r *Recorder) RecordRun(record PluginRunRecord) (string, error) {
	title := fmt.Sprintf("Plugin run: %s", record.PluginName)

	// Build labels
	labels := []string{
		"type:plugin-run",
		fmt.Sprintf("plugin:%s", record.PluginName),
		fmt.Sprintf("result:%s", record.Result),
	}
	if record.RigName != "" {
		labels = append(labels, fmt.Sprintf("rig:%s", record.RigName))
	}

	if r.store != nil {
		return r.recordRunStore(title, labels, record.Body)
	}
	return r.recordRunBd(title, labels, record.Body)
}

// recordRunStore writes a plugin-run bead via the in-process store and
// immediately closes it.
//
// The close is required, not optional: `gt compact`
// (internal/cmd/compact.go:223-236) treats non-closed wisps past TTL with
// no comments and no parent as "proven value" and PROMOTES them to the
// issues table. Without the close, accumulated plugin-run receipts would
// pollute issues forever. Closed wisps are deleted on TTL expiry instead,
// which is the desired behavior.
//
// The provenance string is BD_ACTOR (set to "daemon" by the dispatch
// loop) when present, falling back to "daemon" so created_by is never
// empty — matching the convention used by daemon-spawned bd subprocesses
// (see daemon.go's BD_ACTOR=daemon env wiring at line 2812).
func (r *Recorder) recordRunStore(title string, labels []string, body string) (string, error) {
	ctx, cancel := recorderCtx()
	defer cancel()

	actor := os.Getenv("BD_ACTOR")
	if actor == "" {
		actor = "daemon"
	}

	issue := &beadsdk.Issue{
		Title:       title,
		Description: body,
		Labels:      labels,
		Ephemeral:   true,
	}
	if err := r.store.CreateIssue(ctx, issue, actor); err != nil {
		return "", fmt.Errorf("creating plugin run bead via store: %w", err)
	}

	// Close immediately so the receipt is eligible for closed-wisp cleanup
	// and isn't promoted to a persistent issue past TTL. Best-effort —
	// reaper will catch it if this fails.
	closeCtx, closeCancel := recorderCtx()
	defer closeCancel()
	_ = r.store.CloseIssue(closeCtx, issue.ID, "plugin run recorded", actor, "")

	return issue.ID, nil
}

// recordRunBd writes a plugin-run bead by shelling out to bd. Used when no
// in-process store is attached (CLI invocations, tests).
func (r *Recorder) recordRunBd(title string, labels []string, body string) (string, error) {
	args := []string{
		"create",
		"--ephemeral",
		"--json",
		"--title=" + title,
	}
	for _, label := range labels {
		args = append(args, "-l", label)
	}
	if body != "" {
		args = append(args, "--description="+body)
	}

	ctx, cancel := recorderCtx()
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: bd is a trusted internal tool
	cmd.Dir = r.townRoot
	// Set BEADS_DIR explicitly to prevent inherited env vars from causing
	// prefix mismatches when redirects are in play.
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beads.ResolveBeadsDir(r.townRoot))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("creating plugin run bead: %s: %w", stderr.String(), err)
	}

	// Parse created bead ID from JSON output
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", fmt.Errorf("parsing bd create output: %w", err)
	}

	// Close the receipt immediately so it doesn't get promoted to a
	// persistent issue. `gt compact` (internal/cmd/compact.go:223-236) treats
	// non-closed wisps past TTL with no comments and no parent as
	// "proven value" and PROMOTES them to the issues table — without an
	// immediate close, accumulated plugin-run receipts would pollute issues
	// forever. The close also makes them eligible for `bd mol wisp gc
	// --closed`, the fast cleanup path.
	//
	// The second MySQL session per dispatch is the cost we pay for proper
	// cleanup. The "nothing to commit" log noise this used to produce is
	// silenced by the log_level=error config change in this same PR; the
	// per-session SQL preamble cost is addressed in the Tier 3 SDK
	// migration. (Reverted from the original Tier 1+2 close-removal after
	// augment review pointed out the wisp-promotion side effect.)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer closeCancel()
	closeCmd := exec.CommandContext(closeCtx, "bd", "close", result.ID, "--reason", "plugin run recorded") //nolint:gosec // G204: bd is a trusted internal tool
	closeCmd.Dir = r.townRoot
	closeCmd.Env = append(os.Environ(), "BEADS_DIR="+beads.ResolveBeadsDir(r.townRoot))
	_ = closeCmd.Run() // Best-effort — reaper will catch it if this fails

	return result.ID, nil
}

// GetLastRun returns the most recent run for a plugin.
// Returns nil if no runs found.
func (r *Recorder) GetLastRun(pluginName string) (*PluginRunBead, error) {
	runs, err := r.queryRuns(pluginName, 1, "")
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs[0], nil
}

// GetRunsSince returns all runs for a plugin since the given duration.
// Duration format: "1h", "24h", "7d", etc.
func (r *Recorder) GetRunsSince(pluginName string, since string) ([]*PluginRunBead, error) {
	return r.queryRuns(pluginName, 0, since)
}

// queryRuns queries plugin run beads from the ledger.
func (r *Recorder) queryRuns(pluginName string, limit int, since string) ([]*PluginRunBead, error) {
	if r.store != nil {
		return r.queryRunsStore(pluginName, limit, since)
	}
	return r.queryRunsBd(pluginName, limit, since)
}

// queryRunsStore reads plugin-run beads via the in-process store. Plugin run
// receipts are ephemeral wisps; we filter on Labels (AND) for type:plugin-run
// + plugin:<name> and explicitly include every status (Statuses) so the
// cooldown gate sees closed runs as well as open ones — matching the
// `--all` flag of the bd CLI fallback.
//
// The SDK's IssueFilter.Status defaults to "non-closed only" when nil
// (per its godoc on IssueFilter.Status). Without an explicit Statuses
// list, freshly-recorded plugin runs that the recorder closes (or the
// reaper closes on cleanup) would silently disappear from the cooldown
// query, allowing infinite re-dispatch.
func (r *Recorder) queryRunsStore(pluginName string, limit int, since string) ([]*PluginRunBead, error) {
	ctx, cancel := recorderCtx()
	defer cancel()

	ephemeral := true
	filter := beadsdk.IssueFilter{
		Labels: []string{"type:plugin-run", fmt.Sprintf("plugin:%s", pluginName)},
		Statuses: []beadsdk.Status{
			beadsdk.StatusOpen,
			beadsdk.StatusInProgress,
			beadsdk.StatusBlocked,
			beadsdk.StatusDeferred,
			beadsdk.StatusClosed,
		},
		Ephemeral: &ephemeral,
		Limit:     limit,
	}
	if since != "" {
		// Same unit semantics as the bd path: Go's time.ParseDuration where
		// "m" means minutes (bd's compact format treats "m" as months).
		d, err := time.ParseDuration(since)
		if err != nil {
			return nil, fmt.Errorf("parsing duration %q: %w", since, err)
		}
		cutoff := time.Now().Add(-d).UTC()
		filter.CreatedAfter = &cutoff
	}

	issues, err := r.store.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("querying plugin runs via store: %w", err)
	}

	runs := make([]*PluginRunBead, 0, len(issues))
	for _, si := range issues {
		run := &PluginRunBead{
			ID:        si.ID,
			Title:     si.Title,
			CreatedAt: si.CreatedAt,
			Labels:    si.Labels,
		}
		for _, label := range si.Labels {
			if strings.HasPrefix(label, "result:") {
				run.Result = RunResult(strings.TrimPrefix(label, "result:"))
				break
			}
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// queryRunsBd reads plugin-run beads by shelling out to bd. Used when no
// in-process store is attached.
func (r *Recorder) queryRunsBd(pluginName string, limit int, since string) ([]*PluginRunBead, error) {
	args := []string{
		"list",
		"--json",
		"--all", // Include closed beads too
		"-l", "type:plugin-run",
		"-l", fmt.Sprintf("plugin:%s", pluginName),
	}
	if limit > 0 {
		args = append(args, fmt.Sprintf("--limit=%d", limit))
	}
	if since != "" {
		// Parse as Go duration and compute an absolute RFC3339 cutoff.
		// bd's compact duration uses "m" for months, but plugin gate
		// durations use Go's time.ParseDuration where "m" means minutes.
		// Passing an absolute timestamp avoids this unit mismatch.
		d, err := time.ParseDuration(since)
		if err != nil {
			return nil, fmt.Errorf("parsing duration %q: %w", since, err)
		}
		cutoff := time.Now().Add(-d).UTC().Format(time.RFC3339)
		args = append(args, "--created-after="+cutoff)
	}

	ctx, cancel := recorderCtx()
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: bd is a trusted internal tool
	cmd.Dir = r.townRoot
	// Set BEADS_DIR explicitly to prevent inherited env vars from causing
	// prefix mismatches when redirects are in play.
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beads.ResolveBeadsDir(r.townRoot))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Empty result is OK (no runs found)
		if stderr.Len() == 0 || stdout.String() == "[]\n" {
			return nil, nil
		}
		return nil, fmt.Errorf("querying plugin runs: %s: %w", stderr.String(), err)
	}

	// Parse JSON output
	var beads []struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		CreatedAt string   `json:"created_at"`
		Labels    []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &beads); err != nil {
		// Empty array is valid
		if stdout.String() == "[]\n" || stdout.Len() == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	// Convert to PluginRunBead with parsed result
	runs := make([]*PluginRunBead, 0, len(beads))
	for _, b := range beads {
		run := &PluginRunBead{
			ID:     b.ID,
			Title:  b.Title,
			Labels: b.Labels,
		}

		// Parse created_at
		if t, err := time.Parse(time.RFC3339, b.CreatedAt); err == nil {
			run.CreatedAt = t
		}

		// Extract result from labels
		for _, label := range b.Labels {
			if len(label) > 7 && label[:7] == "result:" {
				run.Result = RunResult(label[7:])
				break
			}
		}

		runs = append(runs, run)
	}

	return runs, nil
}

// CountRunsSince returns the count of runs for a plugin since the given duration.
// This is useful for cooldown gate evaluation.
func (r *Recorder) CountRunsSince(pluginName string, since string) (int, error) {
	runs, err := r.GetRunsSince(pluginName, since)
	if err != nil {
		return 0, err
	}
	return len(runs), nil
}
