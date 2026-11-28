package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/doltport"
)

func TestForcedDoltPortOutranksTownConfig(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".dolt-data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, ".dolt-data", "config.yaml"), []byte("listener:\n  port: 3307\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_PORT", "3307")
	t.Setenv(doltport.ForceEnv, "1")
	if got := resolveDoltPort(town); got != 1 {
		t.Errorf("resolveDoltPort = %d, want the forced 1", got)
	}
	// Without a town root the inherited variables are the only source.
	env := AgentEnv(AgentEnvConfig{Role: "polecat"})
	if env["GT_DOLT_PORT"] != "1" || env["BEADS_DOLT_PORT"] != "1" {
		t.Errorf("agent env ports = %q, %q, want 1", env["GT_DOLT_PORT"], env["BEADS_DOLT_PORT"])
	}
}
