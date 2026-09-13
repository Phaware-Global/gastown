package witness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installMockBDCreateRecorder writes a fake `bd` binary onto PATH that logs
// every invocation (cwd, BEADS_DIR, args) to logPath and succeeds `bd create`
// with a minimal valid issue JSON payload, mirroring the pattern used in
// internal/beads/beads_agent_test.go.
func installMockBDCreateRecorder(t *testing.T, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock bd script is Unix shell")
	}

	binDir := t.TempDir()
	script := `#!/bin/sh
printf 'pwd=%s\n' "$(pwd)" >> "$MOCK_BD_LOG"
printf 'beads_dir=%s\n' "$BEADS_DIR" >> "$MOCK_BD_LOG"
printf 'args=%s\n' "$*" >> "$MOCK_BD_LOG"

cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done

case "$cmd" in
  create)
    printf '{"id":"gt-acme-witness","title":"Witness for acme","status":"open"}\n'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	scriptPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)
}

// TestEnsureWitnessAgentBead_CreatesAgentBead verifies that starting a
// witness ensures its gt:agent bead exists via a `bd create` call carrying
// the canonical ID and the gt:agent label, instead of relying solely on the
// one-time creation at `gt rig add`/`gt rig adopt` time (gt-63db).
func TestEnsureWitnessAgentBead_CreatesAgentBead(t *testing.T) {
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	witnessDir := filepath.Join(townRoot, "acme", "witness")
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		witnessDir,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte(`{"prefix":"gt-","path":"acme/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(townRoot, "bd.log")
	installMockBDCreateRecorder(t, logPath)

	if err := ensureWitnessAgentBead(townRoot, witnessDir, "acme"); err != nil {
		t.Fatalf("ensureWitnessAgentBead: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock bd log: %v", err)
	}
	logOutput := string(logData)
	if !strings.Contains(logOutput, "create --json --id=gt-acme-witness") {
		t.Fatalf("mock bd log missing witness agent bead create call:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "--labels=gt:agent") {
		t.Fatalf("mock bd log missing gt:agent label:\n%s", logOutput)
	}
}
