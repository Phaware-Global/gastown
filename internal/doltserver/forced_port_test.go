package doltserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/doltport"
)

func TestForcedDoltPortOutranksTownConfig(t *testing.T) {
	town := t.TempDir()
	for name, data := range map[string]string{
		".dolt-data/config.yaml": "listener:\n  port: 3307\n",
		"mayor/daemon.json":      `{"env":{"GT_DOLT_PORT":"3307"}}`,
	} {
		path := filepath.Join(town, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GT_DOLT_PORT", "3307")
	t.Setenv(doltport.ForceEnv, "1")
	if got := DefaultConfig(town).Port; got != 1 {
		t.Errorf("Port = %d, want the forced 1", got)
	}
}
