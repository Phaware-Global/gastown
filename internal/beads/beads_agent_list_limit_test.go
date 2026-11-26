package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// bd list returns 50 rows by default, which can truncate the agent beads.
func TestListAgentBeadsDisablesRowLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	binDir := t.TempDir()
	stub := `#!/bin/sh
case " $* " in
  *" mol "*) echo '{"wisps":[]}' ;;
  *" list "*" --limit=0 "*) echo '[{"id":"gt-demo-witness","status":"open","issue_type":"agent","labels":["gt:agent"]}]' ;;
  *" list "*) echo '[]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	agents, err := NewIsolated(dir).ListAgentBeads()
	if err != nil {
		t.Fatalf("ListAgentBeads: %v", err)
	}
	if agents["gt-demo-witness"] == nil {
		t.Fatalf("agents = %v, want gt-demo-witness from an unlimited list", agents)
	}
}
