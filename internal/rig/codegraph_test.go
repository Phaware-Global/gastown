package rig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func writeTownSettings(t *testing.T, townRoot string, s *config.TownSettings) {
	t.Helper()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TownSettingsPath(townRoot), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeRigSettings(t *testing.T, rigPath string, s *config.RigSettings) {
	t.Helper()
	settingsDir := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.RigSettingsPath(rigPath), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCodegraphIndex_DisabledByTown(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(false)},
	})

	// Must not fail even though codegraph binary is not installed.
	if err := EnsureCodegraphIndex(worktree, townRoot, rigPath); err != nil {
		t.Errorf("expected nil error when disabled, got: %v", err)
	}
}

func TestEnsureCodegraphIndex_DisabledByRig(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	// Town enables, rig overrides to disabled.
	writeTownSettings(t, townRoot, &config.TownSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(true)},
	})
	writeRigSettings(t, rigPath, &config.RigSettings{
		CodeGraph: &config.CodeGraphConfig{Enabled: boolPtr(false)},
	})

	if err := EnsureCodegraphIndex(worktree, townRoot, rigPath); err != nil {
		t.Errorf("expected nil error when disabled by rig, got: %v", err)
	}
}

func TestEnsureCodegraphIndex_BinaryMissing(t *testing.T) {
	tmp := t.TempDir()
	townRoot := filepath.Join(tmp, "town")
	rigPath := filepath.Join(townRoot, "rig")
	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}

	// Enabled but codegraph not on PATH — should silently succeed.
	t.Setenv("PATH", tmp) // tmp has no codegraph binary

	if err := EnsureCodegraphIndex(worktree, townRoot, rigPath); err != nil {
		t.Errorf("expected nil error when binary missing, got: %v", err)
	}
}
