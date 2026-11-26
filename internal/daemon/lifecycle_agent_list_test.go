package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// "bd mol wisp list" returns neither hook_bead nor agent_state, which every
// consumer of the agent listing needs, so the listing must not pay for it.
func TestListAgentBeadsJSONSkipsWispList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	stub := `#!/bin/sh
echo "$*" >> "$0.log"
[ "$1" = mol ] && { echo '{"count":0,"wisps":[]}'; exit 0; }
echo '[{"id":"gt-demo-polecat-one","hook_bead":"gt-work","labels":["gt:agent"]}]'
`
	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, bdPath: bdPath}

	var agents []struct {
		ID       string `json:"id"`
		HookBead string `json:"hook_bead"`
	}
	if err := d.listAgentBeadsJSON(&agents); err != nil {
		t.Fatalf("listAgentBeadsJSON: %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "gt-demo-polecat-one" || agents[0].HookBead != "gt-work" {
		t.Errorf("agents = %+v, want the listed polecat with its hook", agents)
	}

	calls, err := os.ReadFile(bdPath + ".log")
	if err != nil {
		t.Fatalf("read bd call log: %v", err)
	}
	if strings.Contains(string(calls), "wisp list") {
		t.Errorf("listing ran the wisp list:\n%s", calls)
	}
}
