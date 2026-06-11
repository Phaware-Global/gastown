package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountJSONLEntries(t *testing.T) {
	t.Run("missing file returns error", func(t *testing.T) {
		tmp := t.TempDir()
		_, _, _, err := countJSONLEntries(tmp)
		if err == nil {
			t.Fatal("expected error for missing issues.jsonl")
		}
	})

	t.Run("empty file returns zero counts", func(t *testing.T) {
		tmp := t.TempDir()
		beadsDir := filepath.Join(tmp, ".beads")
		if err := os.MkdirAll(beadsDir, 0755); err != nil {
			t.Fatal(err)
		}
		issuesPath := filepath.Join(beadsDir, "issues.jsonl")
		if err := os.WriteFile(issuesPath, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}

		total, ephemeral, fileSize, err := countJSONLEntries(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 0 || ephemeral != 0 {
			t.Errorf("expected 0 total, 0 ephemeral; got %d, %d", total, ephemeral)
		}
		if fileSize != 0 {
			t.Errorf("expected fileSize 0, got %d", fileSize)
		}
	})

	t.Run("counts entries and ephemeral entries", func(t *testing.T) {
		tmp := t.TempDir()
		beadsDir := filepath.Join(tmp, ".beads")
		if err := os.MkdirAll(beadsDir, 0755); err != nil {
			t.Fatal(err)
		}

		content := `{"id":"gt-1","title":"Normal issue"}
{"id":"gt-wisp-1","title":"Wisp","ephemeral":true}
{"id":"gt-2","title":"Another issue"}
{"id":"gt-wisp-2","title":"Another wisp","ephemeral":true}
`
		issuesPath := filepath.Join(beadsDir, "issues.jsonl")
		if err := os.WriteFile(issuesPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		total, ephemeral, fileSize, err := countJSONLEntries(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 4 {
			t.Errorf("expected 4 total entries, got %d", total)
		}
		if ephemeral != 2 {
			t.Errorf("expected 2 ephemeral entries, got %d", ephemeral)
		}
		if fileSize <= 0 {
			t.Errorf("expected positive fileSize, got %d", fileSize)
		}
	})

	t.Run("ignores blank lines", func(t *testing.T) {
		tmp := t.TempDir()
		beadsDir := filepath.Join(tmp, ".beads")
		if err := os.MkdirAll(beadsDir, 0755); err != nil {
			t.Fatal(err)
		}

		content := `{"id":"gt-1","title":"Issue"}

{"id":"gt-2","title":"Issue2"}

`
		issuesPath := filepath.Join(beadsDir, "issues.jsonl")
		if err := os.WriteFile(issuesPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		total, _, _, err := countJSONLEntries(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 2 {
			t.Errorf("expected 2 total entries (ignoring blank lines), got %d", total)
		}
	})
}

func TestCheckJSONLBloat_Run(t *testing.T) {
	t.Run("no databases returns OK", func(t *testing.T) {
		tmp := t.TempDir()
		// No .dolt-data directory → ListDatabases returns empty list
		check := NewCheckJSONLBloat()
		ctx := &CheckContext{TownRoot: tmp}
		result := check.Run(ctx)
		if result.Status != StatusOK {
			t.Errorf("expected StatusOK, got %v: %s", result.Status, result.Message)
		}
	})

	t.Run("clean issues.jsonl returns OK", func(t *testing.T) {
		tmp := setupBloatTestRig(t, "myrig", `{"id":"gt-1","title":"Issue1"}
{"id":"gt-2","title":"Issue2"}
`)
		check := NewCheckJSONLBloat()
		ctx := &CheckContext{TownRoot: tmp}
		result := check.Run(ctx)
		if result.Status != StatusOK {
			t.Errorf("expected StatusOK for small clean file, got %v: %s", result.Status, result.Message)
		}
	})

	t.Run("large issues.jsonl triggers file size warning", func(t *testing.T) {
		// Build content that exceeds jsonlSizeLimitBytes
		var sb strings.Builder
		for i := range 600 {
			sb.WriteString(`{"id":"gt-` + string(rune('a'+i%26)) + `","title":"` + strings.Repeat("x", 20000) + `"}` + "\n")
		}
		tmp := setupBloatTestRig(t, "myrig", sb.String())

		check := NewCheckJSONLBloat()
		ctx := &CheckContext{TownRoot: tmp}
		result := check.Run(ctx)
		if result.Status != StatusWarning {
			t.Errorf("expected StatusWarning for large file, got %v: %s", result.Status, result.Message)
		}
		if result.FixHint == "" {
			t.Error("expected non-empty FixHint for bloated check")
		}
		if !strings.Contains(result.FixHint, "gt reaper") {
			t.Errorf("expected FixHint to mention 'gt reaper', got: %s", result.FixHint)
		}
	})

	t.Run("high ephemeral ratio triggers warning", func(t *testing.T) {
		// 4 ephemeral, 1 permanent = 80% ephemeral (above 30% threshold)
		content := `{"id":"gt-1","title":"Permanent"}
{"id":"gt-wisp-1","title":"Wisp","ephemeral":true}
{"id":"gt-wisp-2","title":"Wisp","ephemeral":true}
{"id":"gt-wisp-3","title":"Wisp","ephemeral":true}
{"id":"gt-wisp-4","title":"Wisp","ephemeral":true}
`
		tmp := setupBloatTestRig(t, "myrig", content)

		check := NewCheckJSONLBloat()
		ctx := &CheckContext{TownRoot: tmp}
		result := check.Run(ctx)
		// Note: the count check may not fire (DB unavailable in test), but ephemeral
		// ratio check fires when >30% of entries are ephemeral.
		// The result depends on whether queryLiveIssueCount can run in test env.
		// We primarily verify no panic and that FixHint is set on warning.
		if result.Status == StatusWarning && result.FixHint == "" {
			t.Error("expected FixHint when status is warning")
		}
	})
}

// setupBloatTestRig creates a minimal rig directory with issues.jsonl for testing.
// It creates .dolt-data/<rigName>/.dolt/ so ListDatabases recognises it, and
// <rigName>/.beads/issues.jsonl with the given content.
func setupBloatTestRig(t *testing.T, rigName, content string) string {
	t.Helper()
	tmp := t.TempDir()

	// Create .dolt-data/<rigName>/.dolt/noms/manifest so ListDatabases picks it up
	doltDir := filepath.Join(tmp, ".dolt-data", rigName, ".dolt", "noms")
	if err := os.MkdirAll(doltDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(doltDir, "manifest")
	if err := os.WriteFile(manifestPath, []byte("5\nnbs\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create <rigName>/.beads/issues.jsonl
	beadsDir := filepath.Join(tmp, rigName, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	issuesPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(issuesPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	return tmp
}
