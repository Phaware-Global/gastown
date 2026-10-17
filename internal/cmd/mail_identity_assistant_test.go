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
	// A town is recognised by mayor/town.json.
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"type":"town","version":1,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(town, "assistant")
	decoy := filepath.Join(town, "somerig", "mayor", "rig", "internal", "assistant")
	for _, d := range []string{filepath.Join(dir, "sub"), decoy} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GT_ROLE", "")
	for _, d := range []string{dir, filepath.Join(dir, "sub")} {
		t.Chdir(d)
		if got := detectSenderFromCwd(); got != "assistant/" {
			t.Errorf("detectSenderFromCwd() in %s = %q, want assistant/", d, got)
		}
	}
	// Unanchored "/assistant/" must not match: this path is a rig checkout.
	t.Chdir(decoy)
	if got := detectSenderFromCwd(); got == "assistant/" {
		t.Errorf("detectSenderFromCwd() in %s = assistant/, want a non-assistant identity", decoy)
	}
}
