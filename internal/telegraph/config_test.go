package telegraph

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_MissingFile(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config for missing file")
	}
}

func TestLoadConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `
[telegraph]
enabled = true
listen_addr = ":9000"
buffer_size = 128
nudge_window = "60s"
body_cap = 2048

[telegraph.providers.jira]
enabled = true
secret_env = "JIRA_SECRET"
events = ["jira:issue_created", "jira:issue_updated"]
`
	if err := os.WriteFile(filepath.Join(settingsDir, "telegraph.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if !cfg.Enabled {
		t.Error("expected enabled=true")
	}
	if cfg.ListenAddr != ":9000" {
		t.Errorf("listen_addr: got %s, want :9000", cfg.ListenAddr)
	}
	if cfg.BufferSize != 128 {
		t.Errorf("buffer_size: got %d, want 128", cfg.BufferSize)
	}
	if cfg.NudgeWindow() != 60*time.Second {
		t.Errorf("nudge_window: got %v, want 60s", cfg.NudgeWindow())
	}
	if cfg.BodyCap != 2048 {
		t.Errorf("body_cap: got %d, want 2048", cfg.BodyCap)
	}

	jira, ok := cfg.Providers["jira"]
	if !ok {
		t.Fatal("expected jira provider")
	}
	if !jira.Enabled {
		t.Error("expected jira enabled")
	}
	if jira.SecretEnv != "JIRA_SECRET" {
		t.Errorf("secret_env: got %s", jira.SecretEnv)
	}
	if len(jira.Events) != 2 {
		t.Errorf("events count: got %d, want 2", len(jira.Events))
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Minimal config — all optional fields omitted.
	content := "[telegraph]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(settingsDir, "telegraph.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("default listen_addr: got %s, want %s", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.BufferSize != DefaultBufferSize {
		t.Errorf("default buffer_size: got %d, want %d", cfg.BufferSize, DefaultBufferSize)
	}
	if cfg.NudgeWindow() != DefaultNudgeWindow {
		t.Errorf("default nudge_window: got %v, want %v", cfg.NudgeWindow(), DefaultNudgeWindow)
	}
	if cfg.BodyCap != DefaultBodyCap {
		t.Errorf("default body_cap: got %d, want %d", cfg.BodyCap, DefaultBodyCap)
	}
}

func TestLoadConfig_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "telegraph.toml"), []byte("not valid toml {{{{"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

func TestProviderConfig_ResolveSecret(t *testing.T) {
	t.Setenv("MY_SECRET_VAR", "supersecret")
	p := ProviderConfig{SecretEnv: "MY_SECRET_VAR"}
	secret, err := p.ResolveSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secret != "supersecret" {
		t.Errorf("got %q, want 'supersecret'", secret)
	}
}

func TestProviderConfig_ResolveSecret_Missing(t *testing.T) {
	p := ProviderConfig{SecretEnv: "DOES_NOT_EXIST_XYZ"}
	_, err := p.ResolveSecret()
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
}

func TestProviderConfig_ResolveSecret_NoEnvField(t *testing.T) {
	p := ProviderConfig{}
	_, err := p.ResolveSecret()
	if err == nil {
		t.Fatal("expected error for empty secret_env")
	}
}
