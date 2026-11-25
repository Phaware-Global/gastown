package mail

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// bd list hides type=agent records unless --include-infra is passed.
func TestQueryAgentsInDirIncludesInfraRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	binDir := t.TempDir()
	// The wisp query returns nothing, so only the list call can find the bead.
	stub := `#!/bin/sh
[ "$1" = mol ] && { echo '[]'; exit 0; }
case "$*" in
  *--include-infra*) printf '%s\n' '[{"id":"gt-demo-witness","description":"role_type: witness\nrig: demo\n","status":"open","issue_type":"agent","labels":["gt:agent"]}]' ;;
  *) echo '[]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	agents, err := NewRouterWithTownRoot(townRoot, townRoot).queryAgentsInDir(beadsDir, "")
	if err != nil {
		t.Fatalf("queryAgentsInDir: %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "gt-demo-witness" {
		t.Fatalf("agents = %+v, want the type=agent witness bead", agents)
	}
}
