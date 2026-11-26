package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// bd list hides type=agent records unless --include-infra is passed.
func TestGetWorkersForIssuesIncludesInfraRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	townRoot, _ := makeRoutingTownWorkspace(t)
	for _, dir := range []string{"demo/polecats", "demo/mayor/rig/.beads"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeRoutingBdStub(t, `case "$*" in
  *--include-infra*) echo '[{"id":"gt-demo-polecat-nux","status":"open","issue_type":"agent","hook_bead":"gt-abc","labels":["gt:agent"]}]' ;;
  *) echo '[]' ;;
esac
`)
	chdirConvoyTest(t, townRoot)

	workers := getWorkersForIssues([]string{"gt-abc"})
	if workers["gt-abc"] == nil {
		t.Fatalf("workers = %v, want a worker for gt-abc from its type=agent bead", workers)
	}
}
