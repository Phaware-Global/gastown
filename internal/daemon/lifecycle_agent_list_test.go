package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// bd list hides type=agent records unless --include-infra is passed, and
// returns at most 50 rows unless --limit=0 is passed.
func TestListAgentBeadsJSONListsAllInfraRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	// The wisp query returns nothing, so only the list call can find the beads.
	stub := `#!/bin/sh
[ "$1" = mol ] && { echo '[]'; exit 0; }
first='{"id":"gt-demo-polecat-one","issue_type":"agent","labels":["gt:agent"]}'
second='{"id":"gt-demo-polecat-two","issue_type":"agent","labels":["gt:agent"]}'
case " $* " in
  *" --include-infra "*) ;;
  *) echo '[]'; exit 0 ;;
esac
case " $* " in
  *" --limit=0 "*) echo "[$first,$second]" ;;
  *) echo "[$first]" ;;
esac
`
	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, bdPath: bdPath}

	var agents []struct {
		ID string `json:"id"`
	}
	if err := d.listAgentBeadsJSON(&agents); err != nil {
		t.Fatalf("listAgentBeadsJSON: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %+v, want both type=agent beads", agents)
	}
}
