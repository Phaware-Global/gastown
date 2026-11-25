package mail

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// "bd mol wisp list" returns no description and takes no description filter, so
// agent queries cannot use its results and must not pay for it.
func TestQueryAgentsInDirSkipsWispList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	binDir := t.TempDir()
	stub := `#!/bin/sh
echo "$*" >> "$0.log"
case " $* " in
  *" wisp list "*) echo '{"count":0,"wisps":[]}' ;;
  *) printf '%s\n' '[{"id":"gt-demo-witness","description":"role_type: witness\nrig: demo","status":"open","labels":["gt:agent"]}]' ;;
esac
`
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(stub), 0o700); err != nil {
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
		t.Errorf("agents = %+v, want the listed witness", agents)
	}

	calls, err := os.ReadFile(bdPath + ".log")
	if err != nil {
		t.Fatalf("read bd call log: %v", err)
	}
	if strings.Contains(string(calls), "wisp list") {
		t.Errorf("query ran the wisp list:\n%s", calls)
	}
}
