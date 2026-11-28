package beads

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/doltport"
)

// A forced port outranks what a beads directory records and what the process
// inherited, in every environment gt builds for bd.
func TestForcedDoltPortOutranksBeadsMetadata(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"dolt-server.port": "3307",
		"metadata.json":    `{"dolt_server_port":3307,"dolt_database":"hq"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(doltport.ForceEnv, "1")
	base := []string{"GT_DOLT_PORT=3307", "BEADS_DOLT_PORT=3307"}

	for label, env := range map[string][]string{
		"pinned":           BuildPinnedBDEnv(base, dir),
		"pinned, no dir":   BuildPinnedBDEnv(base, ""),
		"routing":          BuildRoutingBDEnv(base, dir),
		"routing, no dir":  BuildRoutingBDEnv(base, ""),
		"override of dir":  overrideDoltEnvFromBeadsDir(base, dir),
		"override, no dir": overrideDoltEnvFromBeadsDir(base, ""),
	} {
		if got := envValue(env, "BEADS_DOLT_PORT"); got != "1" {
			t.Errorf("%s: BEADS_DOLT_PORT = %q, want 1", label, got)
		}
		if got := envValue(env, "BEADS_DOLT_SERVER_PORT"); got != "" && got != "1" {
			t.Errorf("%s: BEADS_DOLT_SERVER_PORT = %q, want 1 or unset", label, got)
		}
	}
}
