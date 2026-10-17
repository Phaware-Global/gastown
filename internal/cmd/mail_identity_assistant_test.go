package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectSenderFromCwd_Assistant pins the cwd fallback for the Mayor's
// Assistant: commands run from <town>/assistant without GT_ROLE must send mail
// as assistant/, not as the overseer.
func TestDetectSenderFromCwd_Assistant(t *testing.T) {
	town := t.TempDir()
	dir := filepath.Join(town, "assistant")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "sub")} {
		t.Chdir(d)
		if got := detectSenderFromCwd(); got != "assistant/" {
			t.Errorf("detectSenderFromCwd() in %s = %q, want assistant/", d, got)
		}
	}
}
