package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hintStderr is what bd printed when the town's Dolt server was down. The
// deacon read it from `gt patrol new` output and ran `bd dolt start`.
const hintStderr = `Error: failed to open database: Dolt server unreachable at 127.0.0.1:3307: dial tcp 127.0.0.1:3307: connect: connection refused

Dolt server auto-start is disabled (dolt.auto-start: false).
Start the server manually:
  bd dolt start`

// writePatrolShims puts fake gt and bd binaries first on PATH. gt formula list
// either lists the patrol proto or fails with hintStderr; bd always fails with it.
func writePatrolShims(t *testing.T, formulaListFails bool) {
	t.Helper()
	dir := t.TempDir()
	hintFile := filepath.Join(dir, "hint.txt")
	if err := os.WriteFile(hintFile, []byte(hintStderr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gt := "#!/bin/sh\nif [ \"$1 $2\" = \"formula list\" ]; then\n"
	if formulaListFails {
		gt += "  cat '" + hintFile + "' >&2; exit 1\n"
	} else {
		gt += "  echo 'mol-test-patrol  test patrol'; exit 0\n"
	}
	gt += "fi\nexit 0\n"
	bd := "#!/bin/sh\ncat '" + hintFile + "' >&2\nexit 1\n"
	for name, body := range map[string]string{"gt": gt, "bd": bd} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAutoSpawnPatrol_DoesNotAdviseBdDoltStart(t *testing.T) {
	for _, tt := range []struct {
		name             string
		formulaListFails bool
	}{
		{"wisp create fails", false},
		{"formula list fails", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writePatrolShims(t, tt.formulaListFails)
			cfg := PatrolConfig{
				RoleName:      "deacon",
				PatrolMolName: "mol-test-patrol",
				BeadsDir:      t.TempDir(),
				Assignee:      "deacon",
			}

			_, err := autoSpawnPatrol(cfg)
			if err == nil {
				t.Fatal("autoSpawnPatrol succeeded against a failing bd")
			}
			if strings.Contains(err.Error(), "bd dolt start") {
				t.Errorf("patrol error tells the agent to run bd dolt start:\n%v", err)
			}
			if !strings.Contains(err.Error(), "gt dolt status") {
				t.Errorf("patrol error lacks the Gas Town guidance:\n%v", err)
			}
		})
	}
}
