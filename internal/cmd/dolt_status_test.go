package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestReadBeadsRuntimeConfigServerMetadata(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_server_host": "192.0.2.10",
  "dolt_server_port": 4311,
  "dolt_database": "gastown"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cfg, ok := readBeadsRuntimeConfig(beadsDir, townRoot)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Database != "gastown" {
		t.Fatalf("Database = %q, want gastown", cfg.Database)
	}
	if cfg.Host != "192.0.2.10" {
		t.Fatalf("Host = %q, want 192.0.2.10", cfg.Host)
	}
	if cfg.Port != 4311 {
		t.Fatalf("Port = %d, want 4311", cfg.Port)
	}
}

func TestReadBeadsRuntimeConfigDefaultServerAddr(t *testing.T) {
	// Isolate from the process-wide GT_DOLT_PORT that the shared Dolt-container
	// integration harness sets (internal/testutil/doltserver.go) — this test
	// asserts the DefaultPort fallback when metadata omits a port.
	t.Setenv("GT_DOLT_PORT", "")
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "database": "dolt"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cfg, ok := readBeadsRuntimeConfig(beadsDir, townRoot)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != doltserver.DefaultPort {
		t.Fatalf("Port = %d, want default %d", cfg.Port, doltserver.DefaultPort)
	}
}

func TestReadBeadsRuntimeConfigIgnoresEmbeddedMetadata(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "embedded",
  "dolt_database": "gastown"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	if _, ok := readBeadsRuntimeConfig(beadsDir, townRoot); ok {
		t.Fatal("embedded metadata should not be reported as shared-server config")
	}
}

func TestBeadsScopeHint_HQWarnsAgainstGlobal(t *testing.T) {
	townRoot := filepath.Join(string(filepath.Separator), "custom", "town")
	hint := beadsScopeHint("hq", townRoot)

	for _, want := range []string{"database hq", "bd -C " + townRoot, "bd --global", "beads_global"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("beadsScopeHint() missing %q in:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "~/gt") {
		t.Fatalf("beadsScopeHint() should not hardcode ~/gt:\n%s", hint)
	}
}

func TestBeadsScopeHint_NonHQEmpty(t *testing.T) {
	if hint := beadsScopeHint("gastown", "/custom/town"); hint != "" {
		t.Fatalf("beadsScopeHint() = %q, want empty", hint)
	}
}
