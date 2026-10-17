package doctor

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// WispGCCheck reclaims closed wisps and reports stale open ones.
//
// Wisps are ephemeral issues (Wisp: true flag) used for patrol cycles and
// operational workflows. Only CLOSED wisps are ever deleted here. A non-closed
// wisp may be a live merge request, an escalation, an agent, or the molecule
// of a hooked bead; its age says nothing about whether it is abandoned, and
// wisps are dolt_ignore'd so a deletion cannot be recovered. Stale open wisps
// are reported only; the reaper owns their lifecycle.
type WispGCCheck struct {
	FixableCheck
	threshold  time.Duration
	closedRigs map[string]int // rig -> count of closed wisps awaiting reclaim
}

// NewWispGCCheck creates a new wisp GC check with 1 hour staleness threshold
// for the report-only stale-open detection.
func NewWispGCCheck() *WispGCCheck {
	return &WispGCCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "wisp-gc",
				CheckDescription: "Reclaim closed wisps; report stale open wisps (>1h old)",
				CheckCategory:    CategoryCleanup,
			},
		},
		threshold:  1 * time.Hour,
		closedRigs: make(map[string]int),
	}
}

// Run counts closed wisps (fixable) and stale open wisps (report-only) per rig.
func (c *WispGCCheck) Run(ctx *CheckContext) *CheckResult {
	c.closedRigs = make(map[string]int)

	rigs, err := discoverRigs(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to discover rigs",
			Details: []string{err.Error()},
		}
	}

	if len(rigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs configured",
		}
	}

	var closedDetails, staleDetails []string
	totalClosed, totalStale := 0, 0

	for _, rigName := range rigs {
		rigPath := filepath.Join(ctx.TownRoot, rigName)
		closed, stale := c.countWisps(rigPath)
		if closed > 0 {
			c.closedRigs[rigName] = closed
			totalClosed += closed
			closedDetails = append(closedDetails, fmt.Sprintf("%s: %d closed wisp(s)", rigName, closed))
		}
		if stale > 0 {
			totalStale += stale
			staleDetails = append(staleDetails, fmt.Sprintf("%s: %d stale open wisp(s) (report only)", rigName, stale))
		}
	}
	details := append(closedDetails, staleDetails...)

	if totalClosed > 0 {
		msg := fmt.Sprintf("%d closed wisp(s) to reclaim", totalClosed)
		if totalStale > 0 {
			msg += fmt.Sprintf("; %d stale open wisp(s) left untouched", totalStale)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: msg,
			Details: details,
			FixHint: "Run 'gt doctor --fix' to delete closed wisps (open wisps are never deleted)",
		}
	}

	if totalStale > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("%d stale open wisp(s) (>1h old), not auto-fixed; the reaper owns open-wisp cleanup", totalStale),
			Details: details,
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No closed or stale wisps found",
	}
}

// countWisps returns the number of closed wisps and of non-closed wisps last
// updated before the threshold in a rig. Queries the wisps table via
// bd mol wisp list --all (Dolt server is required).
func (c *WispGCCheck) countWisps(rigPath string) (closed, stale int) {
	cmd := exec.Command("bd", "mol", "wisp", "list", "--all", "--json")
	cmd.Dir = rigPath

	output, err := cmd.Output()
	if err != nil {
		// Dolt is the only supported backend — no wisps table means nothing to count.
		return 0, 0
	}

	// Use UTC for cutoff: Dolt stores timestamps in UTC (gt-ty4).
	cutoff := time.Now().UTC().Add(-c.threshold)
	return countWispsFromJSON(output, cutoff)
}

// countWispsFromJSON parses the output of `bd mol wisp list --all --json` and
// returns the number of closed wisps and the number of non-closed wisps last
// updated before cutoff.
//
// bd >=1.0.3 returns {"wisps": [...], "count": N, "schema_version": 1}.
// Returns 0, 0 on parse error.
func countWispsFromJSON(output []byte, cutoff time.Time) (closed, stale int) {
	var wrapper struct {
		Wisps []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Ephemeral bool   `json:"ephemeral"`
			UpdatedAt string `json:"updated_at"`
		} `json:"wisps"`
	}
	if err := json.Unmarshal(output, &wrapper); err != nil {
		return 0, 0
	}

	for _, w := range wrapper.Wisps {
		if w.Status == "closed" {
			closed++
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339, w.UpdatedAt)
		if err != nil {
			continue
		}
		if !updatedAt.IsZero() && updatedAt.Before(cutoff) {
			stale++
		}
	}

	return closed, stale
}

// Fix deletes closed wisps in each rig that has any. It must never run
// bd mol wisp gc without --closed: that mode deletes every non-closed wisp
// older than --age, including live merge-request and molecule wisps.
func (c *WispGCCheck) Fix(ctx *CheckContext) error {
	var lastErr error

	for rigName := range c.closedRigs {
		rigPath := filepath.Join(ctx.TownRoot, rigName)

		cmd := exec.Command("bd", "mol", "wisp", "gc", "--closed", "--force")
		cmd.Dir = rigPath
		if output, err := cmd.CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("%s: %v (%s)", rigName, err, string(output))
		}
	}

	return lastErr
}
