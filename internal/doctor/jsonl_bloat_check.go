package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// jsonlSizeLimitBytes is the file size above which issues.jsonl is considered bloated.
// At this size, bd writes that re-import the JSONL (beads#4128 write path) exceed 3-4s
// each and risk hitting the GT_BD_TIMEOUT_SEC ceiling under Dolt memory pressure.
// Investigation: hq-2dr55.
const jsonlSizeLimitBytes = 10 * 1024 * 1024 // 10 MB

// CheckJSONLBloat detects when issues.jsonl is massively stale compared to the
// live Dolt database — typically caused by ephemeral wisp data accumulating in
// the git-tracked JSONL export. This is warn-only (bd controls the export).
type CheckJSONLBloat struct {
	BaseCheck
}

// NewCheckJSONLBloat creates a new JSONL bloat detection check.
func NewCheckJSONLBloat() *CheckJSONLBloat {
	return &CheckJSONLBloat{
		BaseCheck: BaseCheck{
			CheckName:        "jsonl-bloat",
			CheckDescription: "Detect stale/bloated issues.jsonl vs live database",
			CheckCategory:    CategoryCleanup,
		},
	}
}

// Run compares issues.jsonl entry counts and file sizes with live DB record counts across rigs.
func (c *CheckJSONLBloat) Run(ctx *CheckContext) *CheckResult {
	databases, err := doltserver.ListDatabases(ctx.TownRoot)
	if err != nil || len(databases) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig databases found (skipping JSONL bloat check)",
		}
	}

	var details []string
	bloated := false

	for _, db := range databases {
		rigDir := filepath.Join(ctx.TownRoot, db)
		jsonlCount, ephemeralCount, fileSize, err := countJSONLEntries(rigDir)
		if err != nil {
			continue // No JSONL file for this rig
		}
		if jsonlCount == 0 {
			continue
		}

		// File size check: warn if issues.jsonl exceeds the threshold. At 10MB+, bd's
		// write path (which re-imports the JSONL per call on beads 1.0.3) takes 3-4s each
		// and can exceed the GT_BD_TIMEOUT_SEC ceiling under Dolt memory pressure (hq-2dr55).
		if fileSize >= jsonlSizeLimitBytes {
			details = append(details, fmt.Sprintf(
				"%s: issues.jsonl is %s — bd writes will be slow under Dolt load",
				db, formatBytes(fileSize)))
			bloated = true
		}

		liveCount, err := queryLiveIssueCount(rigDir)
		if err != nil {
			liveCount = 0 // DB not reachable; still check ephemeral ratio
		}

		// Entry count bloat: JSONL has >3x the live issue count. Using 3x instead of 10x
		// because wisps accumulate in the JSONL but are counted separately in the DB.
		if liveCount > 0 && jsonlCount > liveCount*3 {
			details = append(details, fmt.Sprintf(
				"%s: issues.jsonl has %d entries vs %d live DB records (%.0fx)",
				db, jsonlCount, liveCount, float64(jsonlCount)/float64(liveCount)))
			bloated = true
		}

		// Ephemeral ratio: >30% of JSONL entries are ephemeral wisps.
		if jsonlCount > 0 && ephemeralCount*100/jsonlCount > 30 {
			details = append(details, fmt.Sprintf(
				"%s: %d/%d JSONL entries (%d%%) are ephemeral — wisp data polluting git-tracked export",
				db, ephemeralCount, jsonlCount, ephemeralCount*100/jsonlCount))
			bloated = true
		}
	}

	if bloated {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "issues.jsonl contains stale/ephemeral data bloating the git-tracked export",
			Details: details,
			FixHint: "Run 'gt reaper --purge' to remove stale wisps from the live DB (bd will re-export); " +
				"if bd still times out, set GT_BD_TIMEOUT_SEC=180 in your environment",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "issues.jsonl not bloated",
	}
}

// countJSONLEntries counts total and ephemeral entries in issues.jsonl and returns
// the file size in bytes. Returns an error if the file does not exist.
func countJSONLEntries(rigDir string) (total, ephemeral int, fileSize int64, err error) {
	beadsDir := beads.ResolveBeadsDir(rigDir)
	issuesPath := filepath.Join(beadsDir, "issues.jsonl")
	info, err := os.Stat(issuesPath)
	if err != nil {
		return 0, 0, 0, err
	}
	fileSize = info.Size()

	file, err := os.Open(issuesPath)
	if err != nil {
		return 0, 0, fileSize, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer for large descriptions
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		total++

		var issue struct {
			Ephemeral bool `json:"ephemeral"`
		}
		if err := json.Unmarshal([]byte(line), &issue); err == nil && issue.Ephemeral {
			ephemeral++
		}
	}

	return total, ephemeral, fileSize, nil
}

// queryLiveIssueCount returns the total count of issues in the live DB.
// Counts all records (including closed) to match countJSONLEntries which also counts all.
func queryLiveIssueCount(rigDir string) (int, error) {
	cmd := exec.Command("bd", "sql", "--csv", "SELECT COUNT(*) as cnt FROM issues") //nolint:gosec // G204: query is a constant
	cmd.Dir = rigDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("bd sql: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return 0, nil
	}
	cnt := 0
	fmt.Sscanf(strings.TrimSpace(lines[1]), "%d", &cnt)
	return cnt, nil
}
